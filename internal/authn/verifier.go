package authn

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"gitlab.com/ki.yuta1230/kyuusha/internal/audit"
)

type ctxKey struct{}

// Verifier checks bearer JWTs against a fixed ECDSA public key. Swapping
// this for JWKS-based verification against a real OIDC provider later
// doesn't change anything downstream: callers only ever see *Claims via
// FromContext.
type Verifier struct {
	PublicKey *ecdsa.PublicKey
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
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodECDSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Method.Alg())
		}
		return v.PublicKey, nil
	})
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
