package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetricsCollector_CountsAndQuotaUsage covers the two things a real bug
// could plausibly get wrong: mixing two tenants' counts/usage together, and
// dropping the phase/resource label. All three VMs created here stay
// Pending (Create never schedules synchronously post-split -- see
// internal/compute/reconciler.go), so this also implicitly proves the
// collector doesn't require the reconcile loop to actually be running.
func TestMetricsCollector_CountsAndQuotaUsage(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.Create(ctx, "tenant-a", "vm-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 128}); err != nil {
		t.Fatalf("Create vm-1: %v", err)
	}
	if _, err := svc.Create(ctx, "tenant-a", "vm-2", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 128}); err != nil {
		t.Fatalf("Create vm-2: %v", err)
	}
	if _, err := svc.Create(ctx, "tenant-b", "vm-3", VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 256}); err != nil {
		t.Fatalf("Create vm-3: %v", err)
	}

	c := NewMetricsCollector(svc)
	want := `
# HELP kyuusha_tenant_quota_used compute's own tenant_usage accounting (see docs/architecture.md「Quota設計」), by tenant_id and resource (vcpu|memory_mb|vms). The matching limit is exposed by identity as kyuusha_tenant_quota_limit.
# TYPE kyuusha_tenant_quota_used gauge
kyuusha_tenant_quota_used{resource="memory_mb",tenant_id="tenant-a"} 256
kyuusha_tenant_quota_used{resource="memory_mb",tenant_id="tenant-b"} 256
kyuusha_tenant_quota_used{resource="vcpu",tenant_id="tenant-a"} 2
kyuusha_tenant_quota_used{resource="vcpu",tenant_id="tenant-b"} 2
kyuusha_tenant_quota_used{resource="vms",tenant_id="tenant-a"} 2
kyuusha_tenant_quota_used{resource="vms",tenant_id="tenant-b"} 1
# HELP kyuusha_virtualmachines_total Current number of VirtualMachine objects, by tenant_id and phase.
# TYPE kyuusha_virtualmachines_total gauge
kyuusha_virtualmachines_total{phase="Pending",tenant_id="tenant-a"} 2
kyuusha_virtualmachines_total{phase="Pending",tenant_id="tenant-b"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}
