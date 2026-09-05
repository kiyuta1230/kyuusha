package authn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func signES256(t *testing.T, key *ecdsa.PrivateKey, claims Claims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return signed
}

func validClaims(tenantID string) Claims {
	return Claims{
		TenantID: tenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
}

func TestVerifier_StaticKey_AcceptsValidToken(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	v := NewStaticKeyVerifier(&key.PublicKey)

	token := signES256(t, key, validClaims("tenant-a"))
	claims, err := v.parse(token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.TenantID != "tenant-a" {
		t.Fatalf("TenantID = %q, want tenant-a", claims.TenantID)
	}
}

func TestVerifier_StaticKey_RejectsMissingTenantID(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	v := NewStaticKeyVerifier(&key.PublicKey)

	token := signES256(t, key, validClaims(""))
	if _, err := v.parse(token); err == nil {
		t.Fatal("expected error for missing tenant_id, got nil")
	}
}

func TestVerifier_StaticKey_RejectsWrongKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	v := NewStaticKeyVerifier(&otherKey.PublicKey)

	token := signES256(t, key, validClaims("tenant-a"))
	if _, err := v.parse(token); err == nil {
		t.Fatal("expected signature verification failure, got nil")
	}
}

// TestVerifier_AcceptsRS256 locks in that the verifier isn't hardcoded to
// ECDSA: Keycloak's default signing algorithm is RS256, and the algorithm
// allowlist (validSigningMethods) must include it for that to work.
func TestVerifier_AcceptsRS256(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	v := NewStaticKeyVerifier(&key.PublicKey)

	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims("tenant-a")).SignedString(key)
	if err != nil {
		t.Fatalf("sign RS256: %v", err)
	}
	claims, err := v.parse(signed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.TenantID != "tenant-a" {
		t.Fatalf("TenantID = %q, want tenant-a", claims.TenantID)
	}
}

// TestVerifier_RejectsDisallowedAlgorithm guards against algorithm-confusion
// attacks: even though HS256 (symmetric, "signed" with the public key bytes
// misused as an HMAC secret) is a well-known attack against verifiers that
// trust the token's own alg header, the explicit allowlist must reject it
// before the key func is even consulted.
func TestVerifier_RejectsDisallowedAlgorithm(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	v := NewStaticKeyVerifier(&key.PublicKey)

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims("tenant-a")).SignedString([]byte("attacker-controlled"))
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	if _, err := v.parse(signed); err == nil {
		t.Fatal("expected HS256 to be rejected by the algorithm allowlist, got nil")
	}
}
