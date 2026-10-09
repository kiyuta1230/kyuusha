package authn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"slices"
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

func TestVerifier_IssuerAndAudience(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := NewStaticKeyVerifier(&key.PublicKey)
	v.Issuer, v.Audience = "https://idp.example/realms/kyuusha", "kyuusha"
	mint := func(iss string, aud ...string) string {
		c := validClaims("tenant-a")
		c.Issuer, c.Audience = iss, aud
		return signES256(t, key, c)
	}
	if _, err := v.parse(mint("https://idp.example/realms/kyuusha", "account", "kyuusha")); err != nil {
		t.Fatalf("matching iss and aud: %v", err)
	}
	for name, tok := range map[string]string{
		"wrong issuer":   mint("https://idp.example/realms/other", "kyuusha"),
		"no issuer":      mint("", "kyuusha"),
		"wrong audience": mint("https://idp.example/realms/kyuusha", "some-other-app"),
		"no audience":    mint("https://idp.example/realms/kyuusha"),
	} {
		if _, err := v.parse(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Unset, neither is checked.
	open := NewStaticKeyVerifier(&key.PublicKey)
	if _, err := open.parse(mint("anyone")); err != nil {
		t.Fatalf("no iss/aud configured: %v", err)
	}
}

// TestClaims_Roles: the roles claim may be one string or an array, and
// merges with role.
func TestClaims_Roles(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := NewStaticKeyVerifier(&key.PublicKey)
	sign := func(extra jwt.MapClaims) string {
		m := jwt.MapClaims{"tenant_id": "ops", "exp": time.Now().Add(time.Hour).Unix()}
		for k, val := range extra {
			m[k] = val
		}
		s, err := jwt.NewWithClaims(jwt.SigningMethodES256, m).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for name, c := range map[string]struct {
		extra jwt.MapClaims
		want  []string
	}{
		"array":           {jwt.MapClaims{"roles": []string{"network-admin", "viewer"}}, []string{"network-admin", "viewer"}},
		"single string":   {jwt.MapClaims{"roles": "viewer"}, []string{"viewer"}},
		"role and roles":  {jwt.MapClaims{"role": "admin", "roles": []string{"viewer", "admin"}}, []string{"admin", "viewer"}},
		"neither":         {jwt.MapClaims{}, nil},
		"service account": {jwt.MapClaims{"azp": "kyuusha-vpc", "preferred_username": "service-account-kyuusha-vpc", "roles": []string{"network-admin"}}, []string{"network-admin"}},
	} {
		claims, err := v.parse(sign(c.extra))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := claims.AllRoles(); len(got) != len(c.want) || len(got) > 0 && !slices.Equal(got, c.want) {
			t.Errorf("%s: AllRoles = %v, want %v", name, got, c.want)
		}
		if name == "service account" && (claims.AuthorizedParty != "kyuusha-vpc" || claims.PreferredUsername != "service-account-kyuusha-vpc") {
			t.Errorf("azp/preferred_username = %q/%q", claims.AuthorizedParty, claims.PreferredUsername)
		}
	}
	if _, err := v.parse(sign(jwt.MapClaims{"roles": 42})); err == nil {
		t.Error("a non-string roles claim was accepted")
	}
}
