package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/kiyuta1230/kyuusha/internal/authn"
)

func tokenCmd(args []string) {
	if len(args) < 1 || args[0] != "mint" {
		usage()
		return
	}
	tokenMint(args[1:])
}

// tokenMint is dev-only: it signs a token with a local private key instead
// of going through an OIDC provider's token endpoint. See
// hack/devkeys/README.md and docs/specs/authn-authz.md — real deployments
// issue tokens via whatever OIDC platform is connected, not this.
func tokenMint(args []string) {
	fs := flag.NewFlagSet("token mint", flag.ExitOnError)
	keyPath := fs.String("key", "hack/devkeys/jwt-dev.key", "PEM private key to sign with (dev only)")
	tenant := fs.String("tenant", "", "tenant_id claim (required)")
	role := fs.String("role", "", "role claim: \"\" (no cross-tenant power) | admin (every tenant) | storage-admin (every tenant, block-storage RPCs only) | network-admin (every tenant, network RPCs only) | viewer (every tenant, every service, read-only) -- see docs/specs/authn-authz.md")
	tenantRole := fs.String("tenant-role", "", "tenant_role claim, within the caller's own tenant: \"\" (a.k.a. member, full read/write) | viewer (read-only) -- see docs/specs/authn-authz.md")
	roles := fs.String("roles", "", "comma-separated roles claim, on top of -role (e.g. network-admin,viewer: a controller that writes network resources and reads everything else)")
	issuer := fs.String("issuer", "kyuusha-dev", "iss claim (api-gateway checks it when started with -jwt-issuer)")
	audience := fs.String("audience", "kyuusha", "aud claim (api-gateway checks it when started with -jwt-audience)")
	clientID := fs.String("client-id", "", "azp claim: the OIDC client the token was issued to (e.g. a service account's client id) -- recorded in the audit log")
	username := fs.String("username", "", "preferred_username claim -- recorded in the audit log")
	sub := fs.String("sub", "", "sub claim: who is asking (human operator or service account), for audit logging (optional; self-asserted here, see docs/specs/audit-logging.md)")
	ttl := fs.Duration("ttl", time.Hour, "token lifetime")
	fs.Parse(args)

	if *tenant == "" {
		fatal("-tenant is required")
	}

	key, err := authn.LoadECDSAPrivateKeyPEM(*keyPath)
	if err != nil {
		fatal("load signing key: %v", err)
	}

	claims := authn.Claims{
		TenantID:   *tenant,
		Role:       *role,
		Roles:      splitList(*roles),
		TenantRole: *tenantRole,

		AuthorizedParty:   *clientID,
		PreferredUsername: *username,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    *issuer,
			Subject:   *sub,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(*ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	if *audience != "" {
		claims.Audience = jwt.ClaimStrings{*audience}
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(key)
	if err != nil {
		fatal("sign token: %v", err)
	}
	fmt.Println(signed)
}
