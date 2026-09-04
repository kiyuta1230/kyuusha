package kyuusha.authz

import rego.v1

default allow := false

# admin/operator role can act on any tenant. Per docs/architecture.md's authz
# granularity discussion, admin is a role orthogonal to any single tenant,
# not a per-tenant permission.
allow if {
	input.claims.role == "admin"
}

# ordinary tenant-scoped callers may only act on their own tenant's
# resources: the request's tenant_id must match the token's.
allow if {
	input.claims.tenant_id != ""
	input.claims.tenant_id == input.request.tenant_id
}
