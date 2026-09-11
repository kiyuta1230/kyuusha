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
	// Role is the cross-tenant axis: "" (no cross-tenant power) | "admin"
	// (every tenant, every operation) | "storage-admin" (every tenant, but
	// only block-storage RPCs -- see docs/specs/authn-authz.md "将来の拡張").
	Role string `json:"role,omitempty"`
	// TenantRole is the orthogonal within-tenant axis: "" (a.k.a. "member",
	// full read/write of the caller's own tenant) | "viewer" (read-only).
	// Meaningless together with a non-empty Role, which already grants
	// cross-tenant read/write regardless of this field.
	TenantRole string `json:"tenant_role,omitempty"`
	jwt.RegisteredClaims
}

// IsAdmin reports whether the token carries the cross-tenant admin/operator
// role (see docs/architecture.md's authz granularity discussion: admin is a
// role orthogonal to any single tenant, not a tenant-level permission).
func (c *Claims) IsAdmin() bool {
	return c.Role == "admin"
}

// IsStorageAdmin reports whether the token carries the cross-tenant
// storage-admin role (every tenant's block-storage RPCs, nothing else --
// see docs/specs/authn-authz.md "将来の拡張").
func (c *Claims) IsStorageAdmin() bool {
	return c.Role == "storage-admin"
}
