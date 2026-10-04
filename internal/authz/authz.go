// Package authz implements the OPA-based tenant×R/W authorization from
// docs/architecture.md "認証・認可とHypervisor登録": every request already
// authenticated by internal/authn (which populates *authn.Claims in the
// context) is checked against policy.rego. Runs after authn, never before.
package authz

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiyuta1230/kyuusha/internal/audit"
	"github.com/kiyuta1230/kyuusha/internal/authn"
)

//go:embed policy.rego
var policySrc string

// TenantIDGetter is implemented by most kyuusha request messages: they
// carry tenant_id (CreateVirtualMachineRequest, WatchVirtualMachinesRequest,
// ...). A request that does NOT implement it (e.g. identity's
// CreateTenantRequest -- creating a Tenant isn't scoped under any existing
// tenant_id) is treated as carrying an empty tenant_id, which policy.rego's
// per-tenant rule can never match; such requests are authorized only for
// the admin role.
type TenantIDGetter interface {
	GetTenantId() string
}

type Authorizer struct {
	query rego.PreparedEvalQuery
}

func New(ctx context.Context) (*Authorizer, error) {
	query, err := rego.New(
		rego.Query("data.kyuusha.authz.allow"),
		rego.Module("policy.rego", policySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare opa policy: %w", err)
	}
	return &Authorizer{query: query}, nil
}

// authorize returns the caller's claims (nil only in the "authz ran before
// authn" internal-error case) and the request's own tenant_id alongside the
// allow/deny error, so callers can audit-log both the decision and, for a
// successful one, the eventual RPC outcome without recomputing anything.
// fullMethod (e.g. "/kyuusha.blockstorage.v1.StorageConnectionService/Create")
// is classified into rpc.service/rpc.action for policy.rego -- see
// rpcclass.go and docs/specs/authn-authz.md "将来の拡張".
func (a *Authorizer) authorize(ctx context.Context, req any, fullMethod string) (*authn.Claims, string, error) {
	claims, ok := authn.FromContext(ctx)
	if !ok {
		return nil, "", status.Error(codes.Internal, "authz ran before authn")
	}
	var requestTenantID string
	if tenantGetter, ok := req.(TenantIDGetter); ok {
		requestTenantID = tenantGetter.GetTenantId()
	}

	input := map[string]any{
		"claims": map[string]any{
			"tenant_id":   claims.TenantID,
			"role":        claims.Role,
			"tenant_role": claims.TenantRole,
		},
		"request": map[string]any{
			"tenant_id": requestTenantID,
		},
		"rpc": map[string]any{
			"service": rpcService(fullMethod),
			"action":  rpcAction(fullMethod),
		},
	}
	results, err := a.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return claims, requestTenantID, status.Errorf(codes.Internal, "policy evaluation failed: %v", err)
	}
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return claims, requestTenantID, status.Error(codes.PermissionDenied, "no policy decision")
	}
	allowed, _ := results[0].Expressions[0].Value.(bool)
	if !allowed {
		return claims, requestTenantID, status.Error(codes.PermissionDenied, "not authorized for this tenant")
	}
	return claims, requestTenantID, nil
}

func (a *Authorizer) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		claims, requestTenantID, err := a.authorize(ctx, req, info.FullMethod)
		if err != nil {
			auditDenied(ctx, info.FullMethod, requestTenantID, claims, err)
			return nil, err
		}
		resp, err := handler(ctx, req)
		audit.Log(ctx, audit.Record{
			Event: audit.EventRPCCompleted, RPCMethod: info.FullMethod, RequestTenantID: requestTenantID,
			TenantID: claims.TenantID, Sub: claims.Subject, Role: claims.Role, TenantRole: claims.TenantRole, Err: err,
		})
		return resp, err
	}
}

// StreamInterceptor authorizes server-streaming RPCs (Watch) by checking the
// request the first time it's received: protoc-gen-go-grpc's handler for a
// server-streaming method decodes the request via stream.RecvMsg before
// calling into our service code, so wrapping RecvMsg is what actually lets
// us see it. The audit record for the RPC's overall completion is emitted
// once handler returns, i.e. when the stream itself ends (client
// disconnects, Watch's ctx is canceled, etc.), not per message.
func (a *Authorizer) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		wrapped := &authorizedStream{ServerStream: ss, a: a, rpcMethod: info.FullMethod}
		err := handler(srv, wrapped)
		// A denied stream was already audited as authz_denied, same as a
		// denied unary call -- don't log it a second time as completed.
		if wrapped.claims != nil && !wrapped.denied {
			audit.Log(ss.Context(), audit.Record{
				Event: audit.EventRPCCompleted, RPCMethod: info.FullMethod, RequestTenantID: wrapped.requestTenantID,
				TenantID: wrapped.claims.TenantID, Sub: wrapped.claims.Subject, Role: wrapped.claims.Role, TenantRole: wrapped.claims.TenantRole, Err: err,
			})
		}
		return err
	}
}

func auditDenied(ctx context.Context, rpcMethod, requestTenantID string, claims *authn.Claims, err error) {
	if claims == nil {
		return // "authz ran before authn": an internal bug, not a real access attempt
	}
	audit.Log(ctx, audit.Record{
		Event: audit.EventAuthzDenied, RPCMethod: rpcMethod, RequestTenantID: requestTenantID,
		TenantID: claims.TenantID, Sub: claims.Subject, Role: claims.Role, TenantRole: claims.TenantRole, Err: err,
	})
}

type authorizedStream struct {
	grpc.ServerStream
	a         *Authorizer
	rpcMethod string

	checked         bool
	denied          bool
	claims          *authn.Claims
	requestTenantID string
}

func (s *authorizedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if !s.checked {
		s.checked = true
		claims, requestTenantID, err := s.a.authorize(s.Context(), m, s.rpcMethod)
		s.claims, s.requestTenantID = claims, requestTenantID
		if err != nil {
			s.denied = true
			auditDenied(s.Context(), s.rpcMethod, requestTenantID, claims, err)
			return err
		}
	}
	return nil
}
