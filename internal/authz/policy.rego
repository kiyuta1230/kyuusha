package kyuusha.authz

import rego.v1

default allow := false

# admin/operator role can act on any tenant. Per docs/architecture.md's authz
# granularity discussion, admin is a role orthogonal to any single tenant,
# not a per-tenant permission.
# The caller's cross-tenant roles: the token's role and roles claims
# together (see authn.Claims.AllRoles). A token may hold several, e.g.
# ["network-admin", "viewer"] for a controller that writes network
# resources and reads everything else; each rule below grants what its own
# role grants, and they add up.
roles := {r | some r in input.claims.roles}

allow if {
	"admin" in roles
}

# storage-admin is a narrower version of admin, scoped to one service area
# (block-storage: Volume/VolumeAttachment/StorageConnection) rather than
# every RPC -- see docs/specs/authn-authz.md "将来の拡張".
allow if {
	"storage-admin" in roles
	input.rpc.service == "blockstorage"
}

# network-admin mirrors storage-admin, scoped to network (Subnet/
# NetworkInterface) instead -- same pattern, see docs/specs/authn-authz.md
# "将来の拡張".
allow if {
	"network-admin" in roles
	input.rpc.service == "network"
}

# <service>-admin generalizes storage-admin/network-admin to any service
# by naming convention -- in particular to external backends registered on
# api-gateway (e.g. role "vpc-admin" for kyuusha.vpc.v1.*), whose service
# names kyuusha can't list here. See docs/specs/authn-authz.md "将来の拡張".
allow if {
	input.rpc.service != ""
	concat("", [input.rpc.service, "-admin"]) in roles
}

# viewer (role, not tenant_role) is the cross-tenant, cross-service
# counterpart to tenant_role=="viewer" below: admin's reach (including
# unscoped requests like Hypervisor/Tenant lists, which only admin/
# storage-admin/network-admin can otherwise see) but restricted to
# read-only RPCs -- the "auditor" role. See docs/specs/authn-authz.md
# "将来の拡張".
allow if {
	"viewer" in roles
	input.rpc.action == "read"
}

# ordinary tenant-scoped callers may act on their own tenant's resources:
# the request's tenant_id must match the token's. A "viewer" tenant_role
# narrows this to read-only RPCs (Get/List/Watch) -- any other tenant_role
# (default: "" a.k.a. "member") keeps full read/write. Not holding the
# "viewer" role is required too: every token carries some tenant_id (it's a required claim),
# so a global viewer's own nominal tenant_id would otherwise still match
# here and grant it full read/write over that one tenant -- silently
# defeating the "read-only everywhere, no exceptions" guarantee the viewer
# role above exists to make. storage-admin/network-admin deliberately keep
# falling through to this rule for non-{blockstorage,network} RPCs in their
# own tenant (see TestAuthorize_StorageAdmin/NetworkAdmin's last case) --
# only "viewer" itself is excluded here.
allow if {
	input.claims.tenant_id != ""
	input.claims.tenant_id == input.request.tenant_id
	input.claims.tenant_role != "viewer"
	not "viewer" in roles
}

allow if {
	input.claims.tenant_id != ""
	input.claims.tenant_id == input.request.tenant_id
	input.claims.tenant_role == "viewer"
	input.rpc.action == "read"
}
