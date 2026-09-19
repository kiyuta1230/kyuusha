package main

import (
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/kiyuta1230/kyuusha/internal/authn"
)

// signForTest builds a syntactically valid JWT carrying claims, signed with
// an arbitrary key -- fine since resolveTenant (like ParseUnverified itself)
// never checks the signature, only decodes the payload.
func signForTest(t *testing.T, claims authn.Claims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-key"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestResolveTenant(t *testing.T) {
	if got := resolveTenant(""); got != "" {
		t.Fatalf("empty token: got %q, want \"\"", got)
	}
	if got := resolveTenant("not-a-jwt"); got != "" {
		t.Fatalf("garbage token: got %q, want \"\"", got)
	}

	member := signForTest(t, authn.Claims{TenantID: "tenant-a"})
	if got := resolveTenant(member); got != "tenant-a" {
		t.Fatalf("member token (no role): got %q, want tenant-a", got)
	}

	viewerTenantRole := signForTest(t, authn.Claims{TenantID: "tenant-a", TenantRole: "viewer"})
	if got := resolveTenant(viewerTenantRole); got != "tenant-a" {
		t.Fatalf("tenant-scoped viewer (tenant_role, not role): got %q, want tenant-a", got)
	}

	for _, role := range []string{"admin", "storage-admin", "network-admin", "viewer"} {
		crossTenant := signForTest(t, authn.Claims{TenantID: "bootstrap-admin", Role: role})
		if got := resolveTenant(crossTenant); got != "" {
			t.Fatalf("role=%s token: got %q, want \"\" (cross-tenant roles must stay explicit)", role, got)
		}
	}
}
