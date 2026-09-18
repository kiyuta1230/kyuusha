package kyuusha.compute.quota

import rego.v1

# See docs/architecture.md "Quota設計": the limit values (QuotaSpec) live in
# identity, but the used+requested<=max judgement itself runs here, in
# compute, against usage compute tracks locally -- same reasoning as
# internal/authz, on the same OPA foundation.

default allow := false

allow if {
	input.usage.vcpu + input.request.vcpu <= input.limit.max_vcpu
	input.usage.memory_mb + input.request.memory_mb <= input.limit.max_memory_mb
	input.usage.vm_count + 1 <= input.limit.max_vms
	input.request.vcpu <= input.limit.max_vcpu_per_vm
	input.request.memory_mb <= input.limit.max_memory_mb_per_vm
}

# allow_resize is Resize's counterpart to allow: unlike Create, a resize
# never changes vm_count, and the VM being resized already counts toward
# usage under its *old* vcpu/memory_mb -- so the aggregate check is against
# usage+delta (== usage-old+new), not usage+new, while the per-VM check is
# still against the new absolute values.
default allow_resize := false

allow_resize if {
	input.usage.vcpu + input.request.delta_vcpu <= input.limit.max_vcpu
	input.usage.memory_mb + input.request.delta_memory_mb <= input.limit.max_memory_mb
	input.request.new_vcpu <= input.limit.max_vcpu_per_vm
	input.request.new_memory_mb <= input.limit.max_memory_mb_per_vm
}
