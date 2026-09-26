package kyuusha.image.quota

import rego.v1

# See docs/architecture.md "Quota設計": the limit values (QuotaSpec) live in
# identity, but the used+requested<=max judgement itself runs here, in
# image, against usage image tracks locally -- same reasoning as
# internal/authz and internal/compute/quota.rego.

default allow := false

allow if {
	input.usage.image_count + 1 <= input.limit.max_images
}
