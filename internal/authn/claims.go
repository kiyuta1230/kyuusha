// Package authn verifies the bearer JWTs described in docs/architecture.md
// "認証・認可とHypervisor登録": the南北 (KaaS -> api-gateway) path issues a
// JWT (in production, via whatever OIDC platform is connected -- see
// docs/specs/authn-authz.md) carrying tenant_id, and the receiving service
// verifies it locally against a public key/JWKS rather than calling back to
// the issuer per request.
package authn

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the JWT payload kyuusha expects on every request.
type Claims struct {
	TenantID string `json:"tenant_id"`
	// Role and Roles are the cross-tenant axis, read together (see
	// AllRoles): "admin" (every tenant, every operation) |
	// "storage-admin"/"network-admin" (every tenant, but only block-storage/
	// network RPCs respectively) | "<service>-admin" (the same for any
	// service, external backends included) | "viewer" (every tenant, every
	// service, but read-only -- the "auditor" role). No role at all means no
	// cross-tenant power. Role is the original single-valued claim; Roles
	// lets one token hold several (e.g. a controller that writes network
	// resources and only reads the rest: ["network-admin", "viewer"]) --
	// see docs/specs/authn-authz.md.
	Role  string     `json:"role,omitempty"`
	Roles StringList `json:"roles,omitempty"`
	// TenantRole is the orthogonal within-tenant axis: "" (a.k.a. "member",
	// full read/write of the caller's own tenant) | "viewer" (read-only).
	// Meaningless together with a cross-tenant role, which already grants
	// cross-tenant access regardless of this field.
	TenantRole string `json:"tenant_role,omitempty"`
	// AuthorizedParty (OIDC "azp") is the client the token was issued to --
	// for a service account, the account itself -- and PreferredUsername
	// the issuer's human-readable name for the subject (Keycloak:
	// "service-account-<client-id>" for a service account). Neither
	// affects authorization; both go to the audit log, since sub alone is
	// often an opaque UUID.
	AuthorizedParty   string `json:"azp,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	jwt.RegisteredClaims
}

// StringList is a claim that may be either one string or an array of
// strings (an OIDC provider's mapper emits either, depending on whether
// it's configured as multivalued).
type StringList []string

func (l *StringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one == "" {
			*l = nil
		} else {
			*l = StringList{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("roles claim must be a string or an array of strings: %w", err)
	}
	*l = many
	return nil
}

// AllRoles is every cross-tenant role the token holds (Role and Roles
// together, without empties or duplicates, in order).
func (c *Claims) AllRoles() []string {
	var out []string
	for _, r := range append([]string{c.Role}, c.Roles...) {
		if r != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// HasRole reports whether role is among AllRoles.
func (c *Claims) HasRole(role string) bool {
	return slices.Contains(c.AllRoles(), role)
}

// IsAdmin reports whether the token carries the cross-tenant admin/operator
// role (see docs/architecture.md's authz granularity discussion: admin is a
// role orthogonal to any single tenant, not a tenant-level permission).
func (c *Claims) IsAdmin() bool {
	return c.HasRole("admin")
}
