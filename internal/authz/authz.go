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

	"gitlab.com/ki.yuta1230/kyuusha/internal/authn"
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

func (a *Authorizer) authorize(ctx context.Context, req any) error {
	claims, ok := authn.FromContext(ctx)
	if !ok {
		return status.Error(codes.Internal, "authz ran before authn")
	}
	var requestTenantID string
	if tenantGetter, ok := req.(TenantIDGetter); ok {
		requestTenantID = tenantGetter.GetTenantId()
	}

	input := map[string]any{
		"claims": map[string]any{
			"tenant_id": claims.TenantID,
			"role":      claims.Role,
		},
		"request": map[string]any{
			"tenant_id": requestTenantID,
		},
	}
	results, err := a.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return status.Errorf(codes.Internal, "policy evaluation failed: %v", err)
	}
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return status.Error(codes.PermissionDenied, "no policy decision")
	}
	allowed, _ := results[0].Expressions[0].Value.(bool)
	if !allowed {
		return status.Error(codes.PermissionDenied, "not authorized for this tenant")
	}
	return nil
}

func (a *Authorizer) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := a.authorize(ctx, req); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor authorizes server-streaming RPCs (Watch) by checking the
// request the first time it's received: protoc-gen-go-grpc's handler for a
// server-streaming method decodes the request via stream.RecvMsg before
// calling into our service code, so wrapping RecvMsg is what actually lets
// us see it.
func (a *Authorizer) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &authorizedStream{ServerStream: ss, a: a})
	}
}

type authorizedStream struct {
	grpc.ServerStream
	a       *Authorizer
	checked bool
}

func (s *authorizedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if !s.checked {
		s.checked = true
		if err := s.a.authorize(s.Context(), m); err != nil {
			return err
		}
	}
	return nil
}
