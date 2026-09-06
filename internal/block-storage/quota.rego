package kyuusha.blockstorage.quota

import rego.v1

# See docs/architecture.md "Quota設計": the limit values (QuotaSpec) live in
# identity, but the used+requested<=max judgement itself runs here, in
# block-storage, against usage block-storage tracks locally -- same
# reasoning as internal/authz and internal/compute/quota.rego.

default allow := false

allow if {
	input.usage.volume_gb + input.request.size_gb <= input.limit.max_volume_gb
}
