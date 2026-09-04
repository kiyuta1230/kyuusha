package authn

import "context"

// NewContextForTest injects claims the same way UnaryInterceptor/
// StreamInterceptor do, for tests in other packages (e.g. internal/authz)
// that need an authenticated context without standing up a real Verifier.
func NewContextForTest(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, claims)
}
