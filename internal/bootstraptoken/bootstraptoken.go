// Package bootstraptoken implements the zone-scoped hypervisor
// self-registration credential from docs/architecture.md "Hypervisor自己登録と
// zone割当": an operator mints one of these (`kyuusha hypervisor
// bootstrap-token create -zone=...`) with the same signing key
// internal/authn's dev-mode client JWTs use, and compute-agent presents it
// when calling RegisterHypervisor. compute trusts the token's zone claim,
// never the agent's own self-reported zone -- a compromised or
// misconfigured agent can't claim capacity in a zone it wasn't provisioned
// into.
//
// This is deliberately narrower than the full design docs/architecture.md
// describes: it authenticates claims the token carries, not the
// hypervisor's cryptographic identity, and issues no follow-up mTLS client
// certificate (every service already shares one mTLS identity via
// internal/mtls, so there's no per-hypervisor credential to hand out).
// HypervisorID (2026-09-11) is optional, not the token's primary axis: the
// zone claim alone is still meant to be shared across every hypervisor
// PXE/cloud-init-provisioned into the same zone with the same image, so
// tokens still aren't single-use in the general case. An operator who
// mints one *with* a HypervisorID gets individual identity (RegisterHypervisor
// rejects a request whose own hypervisor field doesn't match it) and the
// ability to revoke that specific id later (HypervisorSpec.revoked) -- but
// still no continuous per-connection authentication beyond the shared mTLS,
// see docs/specs/hypervisor-bootstrap.md for the deliberate scope
// boundary.
package bootstraptoken

import (
	"crypto"
	"crypto/ecdsa"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// validSigningMethods mirrors internal/authn's algorithm allowlist for the
// same alg-confusion-attack reason: never trust the token's own "alg" header.
var validSigningMethods = []string{"ES256", "RS256"}

// Claims is a bootstrap token's payload.
type Claims struct {
	Zone string `json:"zone"`
	// HypervisorID, if set, restricts this token to registering exactly one
	// hypervisor id -- see the package doc comment.
	HypervisorID string `json:"hypervisor_id,omitempty"`
	jwt.RegisteredClaims
}

// Mint signs a new bootstrap token authorizing self-registration into zone,
// valid for ttl. hypervisorID is optional (see the package doc comment);
// pass "" for the original zone-only, shareable-across-a-fleet behavior.
// Dev/CLI-only (`kyuusha hypervisor bootstrap-token create`); nothing in a
// running kyuusha service holds a signing key.
func Mint(key *ecdsa.PrivateKey, zone, hypervisorID string, ttl time.Duration) (string, error) {
	if zone == "" {
		return "", errors.New("zone is required")
	}
	claims := Claims{
		Zone:         zone,
		HypervisorID: hypervisorID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(key)
}

// VerifyWithKey verifies tokenStr against a single fixed public key (the
// only key source this package's callers need -- same devkeys precedent as
// internal/authn.NewStaticKeyVerifier) and returns its claims.
func VerifyWithKey(pubKey crypto.PublicKey, tokenStr string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(*jwt.Token) (any, error) {
		return pubKey, nil
	}, jwt.WithValidMethods(validSigningMethods))
	if err != nil {
		return nil, err
	}
	if claims.Zone == "" {
		return nil, errors.New("bootstrap token missing required zone claim")
	}
	return claims, nil
}
