package kyuusha.authz

import rego.v1

default allow := false

# admin/operator role can act on any tenant. Per docs/architecture.md's authz
# granularity discussion, admin is a role orthogonal to any single tenant,
# not a per-tenant permission.
allow if {
	input.claims.role == "admin"
}

# storage-admin is a narrower version of admin, scoped to one service area
# (block-storage: Volume/VolumeAttachment/StorageConnection) rather than
# every RPC -- see docs/specs/authn-authz.md "将来の拡張".
allow if {
	input.claims.role == "storage-admin"
	input.rpc.service == "blockstorage"
}

# ordinary tenant-scoped callers may act on their own tenant's resources:
# the request's tenant_id must match the token's. A "viewer" tenant_role
# narrows this to read-only RPCs (Get/List/Watch) -- any other tenant_role
# (default: "" a.k.a. "member") keeps full read/write.
allow if {
	input.claims.tenant_id != ""
	input.claims.tenant_id == input.request.tenant_id
	input.claims.tenant_role != "viewer"
}

allow if {
	input.claims.tenant_id != ""
	input.claims.tenant_id == input.request.tenant_id
	input.claims.tenant_role == "viewer"
	input.rpc.action == "read"
}
