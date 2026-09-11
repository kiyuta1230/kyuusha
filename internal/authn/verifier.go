package authn

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kiyuta1230/kyuusha/internal/audit"
)

type ctxKey struct{}

// validSigningMethods is the algorithm allowlist enforced regardless of key
// source: a JWT's own "alg" header is never trusted blindly (the classic
// algorithm-confusion attack), only these two, matching the two issuers this
// has actually been run against -- the dev-only ES256 signer (hack/devkeys)
// and Keycloak's RS256 default. Add to this list, don't remove the check,
// if another issuer needs a different algorithm.
var validSigningMethods = []string{"ES256", "RS256"}

// Verifier checks bearer JWTs. Callers only ever see *Claims via
// FromContext regardless of which key source below produced it.
type Verifier struct {
	KeyFunc jwt.Keyfunc
}

// NewStaticKeyVerifier builds a Verifier that always verifies against one
// fixed public key -- e.g. loaded from a PEM file via
// LoadECDSAPublicKeyPEM. There's no real OIDC provider to fetch a JWKS from
// in dev/test/playground use, so this is what hack/devkeys pairs with.
func NewStaticKeyVerifier(key crypto.PublicKey) *Verifier {
	return &Verifier{KeyFunc: func(*jwt.Token) (any, error) { return key, nil }}
}

// NewJWKSVerifier builds a Verifier that resolves each token's verification
// key from a JWKS endpoint (the standard OIDC key-publication mechanism --
// e.g. Keycloak's .../protocol/openid-connect/certs), keyed by the token's
// kid header, with the key set cached and refreshed automatically.
func NewJWKSVerifier(ctx context.Context, jwksURL string) (*Verifier, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("fetch jwks from %s: %w", jwksURL, err)
	}
	return &Verifier{KeyFunc: k.Keyfunc}, nil
}

// FromContext returns the authenticated caller's claims. Only meaningful
// inside a handler reached through UnaryInterceptor/StreamInterceptor.
func FromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Claims)
	return c, ok
}

func (v *Verifier) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		claims, err := v.authenticate(ctx)
		if err != nil {
			audit.Log(ctx, audit.Record{Event: audit.EventAuthnFailed, RPCMethod: info.FullMethod, Err: err})
			return nil, err
		}
		return handler(context.WithValue(ctx, ctxKey{}, claims), req)
	}
}

func (v *Verifier) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		claims, err := v.authenticate(ss.Context())
		if err != nil {
			audit.Log(ss.Context(), audit.Record{Event: audit.EventAuthnFailed, RPCMethod: info.FullMethod, Err: err})
			return err
		}
		return handler(srv, &authenticatedStream{
			ServerStream: ss,
			ctx:          context.WithValue(ss.Context(), ctxKey{}, claims),
		})
	}
}

func (v *Verifier) authenticate(ctx context.Context) (*Claims, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing authorization header")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(values[0], prefix) {
		return nil, status.Error(codes.Unauthenticated, "authorization header must be a Bearer token")
	}

	claims, err := v.parse(strings.TrimPrefix(values[0], prefix))
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid token: %v", err)
	}
	return claims, nil
}

func (v *Verifier) parse(token string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims, v.KeyFunc, jwt.WithValidMethods(validSigningMethods))
	if err != nil {
		return nil, err
	}
	if claims.TenantID == "" {
		return nil, errors.New("token missing required tenant_id claim")
	}
	return claims, nil
}

type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context { return s.ctx }
