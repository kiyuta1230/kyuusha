package authn

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// The client-facing JWT (verified once, at api-gateway, by
// Verifier.UnaryInterceptor/StreamInterceptor) never reaches backend
// services -- api-gateway is the only thing that ever sees it. What
// backends need for fine-grained authorization decisions (e.g. "did the
// same caller who added this finalizer request its removal?") is just the
// caller's identity, not the token itself. These two trusted metadata keys
// carry that identity across the api-gateway -> backend hop, exactly like
// the TenantID a proxy already forwards as an RPC field -- backends trust
// them unconditionally because they trust api-gateway unconditionally (see
// docs/architecture.md's mTLS follow-up: this whole perimeter-security
// model, not just this one hop, is what that closes).
const (
	metadataKeyCallerSub   = "x-kyuusha-caller-sub"
	metadataKeyCallerAdmin = "x-kyuusha-caller-admin"
)

// PropagateCallerUnaryInterceptor forwards the authenticated caller's
// identity (see FromContext) as outgoing gRPC metadata. Attach it once to
// each of api-gateway's backend client connections (cmd/api-gateway/main.go)
// -- every proxy's ctx already carries the caller's Claims (set by the
// server-side Verifier interceptor upstream of it), so no per-proxy code is
// needed.
func PropagateCallerUnaryInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(withCallerMetadata(ctx), method, req, reply, cc, opts...)
	}
}

// PropagateCallerStreamInterceptor is PropagateCallerUnaryInterceptor's
// streaming-RPC counterpart.
func PropagateCallerStreamInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(withCallerMetadata(ctx), desc, cc, method, opts...)
	}
}

func withCallerMetadata(ctx context.Context) context.Context {
	claims, ok := FromContext(ctx)
	if !ok || claims.Subject == "" {
		return ctx
	}
	adminValue := "false"
	if claims.IsAdmin() {
		adminValue = "true"
	}
	return metadata.AppendToOutgoingContext(ctx, metadataKeyCallerSub, claims.Subject, metadataKeyCallerAdmin, adminValue)
}

// CallerSubFromContext returns the propagated caller's JWT `sub`, as
// forwarded by PropagateCallerUnaryInterceptor/PropagateCallerStreamInterceptor.
// Only meaningful on a backend reached through api-gateway; ok is false for
// any other caller (nothing to trust) or when the original token had no
// subject.
func CallerSubFromContext(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get(metadataKeyCallerSub)
	if len(values) == 0 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

// CallerIsAdminFromContext reports whether the propagated caller (see
// CallerSubFromContext) held the admin role.
func CallerIsAdminFromContext(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	values := md.Get(metadataKeyCallerAdmin)
	return len(values) > 0 && values[0] == "true"
}

// ContextWithPropagatedCallerForTest builds an incoming context carrying the
// same metadata PropagateCallerUnaryInterceptor/PropagateCallerStreamInterceptor
// would have attached, for backend-service tests (e.g. an ownership check
// keyed off CallerSubFromContext) that need to simulate a caller identity
// without standing up a real gRPC round-trip through api-gateway.
func ContextWithPropagatedCallerForTest(ctx context.Context, sub string, isAdmin bool) context.Context {
	adminValue := "false"
	if isAdmin {
		adminValue = "true"
	}
	return metadata.NewIncomingContext(ctx, metadata.Pairs(metadataKeyCallerSub, sub, metadataKeyCallerAdmin, adminValue))
}
