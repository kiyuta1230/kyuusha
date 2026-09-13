package compute

import (
	"context"
	"errors"
	"testing"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestService_CreateRejectsUnknownTenant(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	_, err := svc.Create(ctx, "", "x", VirtualMachineSpec{ImageID: "img-abc"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("empty tenant_id: got %v, want ErrValidation", err)
	}
}

func TestService_CreateEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu:          3,
		MaxMemoryMb:      8192,
		MaxVms:           2,
		MaxVcpuPerVm:     2,
		MaxMemoryMbPerVm: 4096,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	// Exceeds max_vcpu_per_vm even though tenant totals have room.
	if _, err := svc.Create(ctx, tenant, "", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 3, MemoryMB: 1024,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over per-VM cap: got %v, want ErrQuotaExceeded", err)
	}

	first, err := svc.Create(ctx, tenant, "vm-1", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 2, MemoryMB: 4096,
	})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Tenant total vcpu would go to 4 > max_vcpu=3.
	if _, err := svc.Create(ctx, tenant, "vm-2", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 2, MemoryMB: 2048,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over tenant total: got %v, want ErrQuotaExceeded", err)
	}

	// A second VM that fits should still succeed.
	second, err := svc.Create(ctx, tenant, "vm-3", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 1, MemoryMB: 1024,
	})
	if err != nil {
		t.Fatalf("second Create (within remaining quota): %v", err)
	}

	// Idempotent re-Create of the first VM must not re-charge quota (a third
	// distinct VM would otherwise now fit: used vcpu=3 == max_vcpu=3, no
	// room; but re-Create of an existing name must succeed regardless).
	again, err := svc.Create(ctx, tenant, "vm-1", VirtualMachineSpec{ImageID: "img-abc"})
	if err != nil {
		t.Fatalf("idempotent re-Create: %v", err)
	}
	if again.Meta.ID != first.Meta.ID {
		t.Fatalf("idempotent re-Create minted a new ID: %s vs %s", again.Meta.ID, first.Meta.ID)
	}

	// max_vms=2 already reached (vm-1, vm-3); a third distinct VM must be
	// rejected even though vcpu/memory alone would fit.
	if _, err := svc.Create(ctx, tenant, "vm-4", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 0, MemoryMB: 0,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over max_vms: got %v, want ErrQuotaExceeded", err)
	}

	// Deleting one VM frees enough quota (vcpu and vm count) for another.
	if err := svc.Delete(ctx, tenant, second.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Create(ctx, tenant, "vm-5", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 1, MemoryMB: 1024,
	}); err != nil {
		t.Fatalf("Create after Delete freed quota: %v", err)
	}
}

// TestService_NewServiceRebuildsUsageFromExistingVirtualMachines proves the
// real bug found 2026-09-13 (same class as
// network.Service.rebuildPools/blockstorage.Service.rebuildUsage, and
// present here even before this session's compute-reconciler split):
// usage is purely in-memory, so without rebuildUsage, a restart forgets
// every tenant's real quota usage and could let Create approve a request
// a live tenant_usage would have rejected. Constructs two Services against
// the SAME etcd client/namespace (not resourcetest.Client(t) called
// twice, which would give each its own isolated namespace) to simulate a
// real restart.
func TestService_NewServiceRebuildsUsageFromExistingVirtualMachines(t *testing.T) {
	ctx := context.Background()
	etcdClient := resourcetest.Client(t)
	quota := &identityv1.QuotaSpec{
		MaxVcpu: 4, MaxMemoryMb: 8192, MaxVms: 10, MaxVcpuPerVm: 4, MaxMemoryMbPerVm: 8192,
	}
	const tenant = "tenant-a"

	svc1, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService (first): %v", err)
	}
	if _, err := svc1.Create(ctx, tenant, "vm-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 3, MemoryMB: 4096}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc2, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService (second, simulating a restart): %v", err)
	}

	// max_vcpu=4, vm-1 already used 3 -- only 1 more vcpu should fit.
	// Without rebuildUsage, svc2 would start from an empty usage map and
	// wrongly allow this.
	if _, err := svc2.Create(ctx, tenant, "vm-2", VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Create (after restart, over remaining vcpu headroom): got %v, want ErrQuotaExceeded -- rebuildUsage did not restore usage's state", err)
	}
	if _, err := svc2.Create(ctx, tenant, "vm-3", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 1024}); err != nil {
		t.Fatalf("Create (after restart, within remaining vcpu headroom): %v", err)
	}
}
