// Package authn verifies the bearer JWTs described in docs/architecture.md
// "認証・認可とHypervisor登録": the南北 (KaaS -> api-gateway) path issues a
// JWT (in production, via whatever OIDC platform is connected -- see
// docs/specs/authn-authz.md) carrying tenant_id, and the receiving service
// verifies it locally against a public key/JWKS rather than calling back to
// the issuer per request.
package authn

import "github.com/golang-jwt/jwt/v5"

// Claims is the JWT payload kyuusha expects on every request.
type Claims struct {
	TenantID string `json:"tenant_id"`
	Role     string `json:"role,omitempty"` // "" (tenant-scoped) | "admin"
	jwt.RegisteredClaims
}

// IsAdmin reports whether the token carries the cross-tenant admin/operator
// role (see docs/architecture.md's authz granularity discussion: admin is a
// role orthogonal to any single tenant, not a tenant-level permission).
func (c *Claims) IsAdmin() bool {
	return c.Role == "admin"
}
