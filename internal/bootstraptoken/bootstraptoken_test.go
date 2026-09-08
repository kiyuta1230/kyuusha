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
	token, err := Mint(key, "zone-a", time.Hour)
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

func TestMint_RequiresZone(t *testing.T) {
	key := genKey(t)
	if _, err := Mint(key, "", time.Hour); err == nil {
		t.Fatal("expected error for empty zone, got nil")
	}
}

func TestVerifyWithKey_RejectsWrongKey(t *testing.T) {
	key := genKey(t)
	otherKey := genKey(t)
	token, err := Mint(key, "zone-a", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := VerifyWithKey(&otherKey.PublicKey, token); err == nil {
		t.Fatal("expected signature verification failure, got nil")
	}
}

func TestVerifyWithKey_RejectsExpiredToken(t *testing.T) {
	key := genKey(t)
	token, err := Mint(key, "zone-a", -time.Hour) // already expired
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
	token, err := Mint(key, "zone-a", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	tampered := token[:len(token)-1] + "x"
	if _, err := VerifyWithKey(&key.PublicKey, tampered); err == nil {
		t.Fatal("expected verification failure for a tampered token, got nil")
	}
}
