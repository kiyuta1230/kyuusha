package bootstraptoken

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"
)

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func TestMintAndVerify_RoundTrip(t *testing.T) {
	key := genKey(t)
	token, err := Mint(key, "zone-a", "", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := VerifyWithKey(&key.PublicKey, token)
	if err != nil {
		t.Fatalf("VerifyWithKey: %v", err)
	}
	if claims.Zone != "zone-a" {
		t.Fatalf("Zone = %q, want zone-a", claims.Zone)
	}
}

// TestMintAndVerify_WithHypervisorID exercises the optional per-hypervisor
// scoping (2026-09-11, docs/specs/hypervisor-bootstrap.md): a token minted
// with a hypervisor_id round-trips it, alongside the zone.
func TestMintAndVerify_WithHypervisorID(t *testing.T) {
	key := genKey(t)
	token, err := Mint(key, "zone-a", "hypervisor-1", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := VerifyWithKey(&key.PublicKey, token)
	if err != nil {
		t.Fatalf("VerifyWithKey: %v", err)
	}
	if claims.Zone != "zone-a" {
		t.Fatalf("Zone = %q, want zone-a", claims.Zone)
	}
	if claims.HypervisorID != "hypervisor-1" {
		t.Fatalf("HypervisorID = %q, want hypervisor-1", claims.HypervisorID)
	}
}

// TestMintAndVerify_WithoutHypervisorID confirms the original, still-valid
// fleet-shareable shape (no hypervisor_id claim at all) keeps working
// unchanged.
func TestMintAndVerify_WithoutHypervisorID(t *testing.T) {
	key := genKey(t)
	token, err := Mint(key, "zone-a", "", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := VerifyWithKey(&key.PublicKey, token)
	if err != nil {
		t.Fatalf("VerifyWithKey: %v", err)
	}
	if claims.HypervisorID != "" {
		t.Fatalf("HypervisorID = %q, want empty", claims.HypervisorID)
	}
}

func TestMint_RequiresZone(t *testing.T) {
	key := genKey(t)
	if _, err := Mint(key, "", "", time.Hour); err == nil {
		t.Fatal("expected error for empty zone, got nil")
	}
}

func TestVerifyWithKey_RejectsWrongKey(t *testing.T) {
	key := genKey(t)
	otherKey := genKey(t)
	token, err := Mint(key, "zone-a", "", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := VerifyWithKey(&otherKey.PublicKey, token); err == nil {
		t.Fatal("expected signature verification failure, got nil")
	}
}

func TestVerifyWithKey_RejectsExpiredToken(t *testing.T) {
	key := genKey(t)
	token, err := Mint(key, "zone-a", "", -time.Hour) // already expired
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := VerifyWithKey(&key.PublicKey, token); err == nil {
		t.Fatal("expected expiry failure, got nil")
	}
}

// TestVerifyWithKey_RejectsForgedZoneWithoutResigning documents the actual
// security property: an attacker who can't sign can't just edit the zone
// claim of an otherwise-valid token, since that invalidates the signature.
func TestVerifyWithKey_RejectsTamperedToken(t *testing.T) {
	key := genKey(t)
	token, err := Mint(key, "zone-a", "", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Tamper a character in the middle of the token, not the last character
	// of the whole string: a base64url segment's trailing character can
	// encode as few as 2 real bits (Go's non-strict decoder ignores the
	// rest as padding), so mutating just that one had a real ~1-in-4 chance
	// of decoding to the exact same bytes as before -- silently tampering
	// nothing at all and making this test flaky (confirmed: it did flake
	// intermittently). A middle character sits deep inside a segment's
	// full 6-bits-per-character body, where any substitution is guaranteed
	// to change the decoded bytes.
	i := len(token) / 2
	replacement := byte('x')
	if token[i] == 'x' {
		replacement = 'y'
	}
	tampered := token[:i] + string(replacement) + token[i+1:]
	if _, err := VerifyWithKey(&key.PublicKey, tampered); err == nil {
		t.Fatal("expected verification failure for a tampered token, got nil")
	}
}
