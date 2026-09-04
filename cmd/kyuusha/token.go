package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"gitlab.com/ki.yuta1230/kyuusha/internal/authn"
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
// hack/devkeys/README.md and docs/architecture.md's auth design — real
// deployments issue tokens via Dex/Hydra, not this.
func tokenMint(args []string) {
	fs := flag.NewFlagSet("token mint", flag.ExitOnError)
	keyPath := fs.String("key", "hack/devkeys/jwt-dev.key", "PEM private key to sign with (dev only)")
	tenant := fs.String("tenant", "", "tenant_id claim (required)")
	role := fs.String("role", "", "role claim, e.g. admin (optional)")
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
		TenantID: *tenant,
		Role:     *role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(*ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodES256, claims).SignedString(key)
	if err != nil {
		fatal("sign token: %v", err)
	}
	fmt.Println(signed)
}
