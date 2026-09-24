package compute

import (
	"context"
	"errors"
	"testing"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestReconciler_ResizeWithMigrationRequiresStopped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	vm, err := svc.Create(ctx, "tenant-a", "", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := r.ResizeWithMigration(ctx, "tenant-a", vm.Meta.ID, 2, 1024); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("ResizeWithMigration on a Pending vm: got %v, want ErrInvalidPhase", err)
	}
}

// TestReconciler_ResizeWithMigrationMovesToHypervisorWithRoom is the main
// success path: hypervisor-1 (the VM's current one) has no room for the new
// size, hypervisor-2 does -- the VM should end up there, at the new size,
// with capacity correctly moved between the two Hypervisors.
func TestReconciler_ResizeWithMigrationMovesToHypervisorWithRoom(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	// hypervisor-1: 4 allocatable, 2 already allocated to this VM -- only 2
	// more vCPU free, not enough to grow 2->8.
	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 4, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 4096})
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}
	deletesTo1 := subscribeDeleteCommands(t, ctx, r.js, "hypervisor-1")

	// Sanity: the plain cold path really does fail first (this is the
	// precondition grpcserver.Resize checks before ever calling
	// ResizeWithMigration).
	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 8, 4096); !errors.Is(err, ErrHypervisorCapacityExceeded) {
		t.Fatalf("plain Resize: got %v, want ErrHypervisorCapacityExceeded", err)
	}

	out, err := r.ResizeWithMigration(ctx, tenant, vm.Meta.ID, 8, 4096)
	if err != nil {
		t.Fatalf("ResizeWithMigration: %v", err)
	}
	if out.Spec.VCPU != 8 || out.Spec.MemoryMB != 4096 {
		t.Fatalf("Spec after ResizeWithMigration = %+v, want vcpu=8 memory_mb=4096", out.Spec)
	}
	if out.Status.Hypervisor != "hypervisor-2" {
		t.Fatalf("Hypervisor = %q, want hypervisor-2", out.Status.Hypervisor)
	}

	old, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-1: %v", err)
	}
	if old.Status.AllocatedVCPU != 0 || old.Status.AllocatedMemoryMB != 0 {
		t.Fatalf("hypervisor-1 allocated = (%d, %d), want fully released", old.Status.AllocatedVCPU, old.Status.AllocatedMemoryMB)
	}
	newH, err := svc.GetHypervisor(ctx, "hypervisor-2")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-2: %v", err)
	}
	if newH.Status.AllocatedVCPU != 8 || newH.Status.AllocatedMemoryMB != 4096 {
		t.Fatalf("hypervisor-2 allocated = (%d, %d), want (8, 4096) -- the *new* size, not a delta", newH.Status.AllocatedVCPU, newH.Status.AllocatedMemoryMB)
	}

	waitFor(t, func() bool { return len(*deletesTo1) == 1 })
	if (*deletesTo1)[0].VMID != vm.Meta.ID {
		t.Fatalf("DeleteCommand to hypervisor-1's vm_id = %q, want %q", (*deletesTo1)[0].VMID, vm.Meta.ID)
	}
}

// TestReconciler_ResizeWithMigrationFailsWhenNoHypervisorFitsEither confirms
// the fallback surfaces the same ErrHypervisorCapacityExceeded a plain
// Resize would have, rather than a confusing ErrUnschedulable, when
// migrating doesn't help either -- and that nothing was mutated.
func TestReconciler_ResizeWithMigrationFailsWhenNoHypervisorFitsEither(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 4, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 4096})
	// hypervisor-2 exists but is also too small for the requested size.
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 4, 8192, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}

	if _, err := r.ResizeWithMigration(ctx, tenant, vm.Meta.ID, 8, 4096); !errors.Is(err, ErrHypervisorCapacityExceeded) {
		t.Fatalf("ResizeWithMigration with no hypervisor big enough: got %v, want ErrHypervisorCapacityExceeded", err)
	}

	stored, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Spec.VCPU != 2 || stored.Status.Hypervisor != "hypervisor-1" {
		t.Fatalf("VM mutated after a failed ResizeWithMigration: %+v", stored)
	}
	old, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-1: %v", err)
	}
	if old.Status.AllocatedVCPU != 2 {
		t.Fatalf("hypervisor-1 allocated vcpu = %d, want still 2 (unchanged)", old.Status.AllocatedVCPU)
	}
}

// TestReconciler_ResizeWithMigrationEnforcesQuota confirms a quota
// rejection short-circuits before any scheduling attempt -- moving
// Hypervisors can never fix a tenant-quota rejection.
func TestReconciler_ResizeWithMigrationEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu: 4, MaxMemoryMb: 8192, MaxVms: 8, MaxVcpuPerVm: 8, MaxMemoryMbPerVm: 8192,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 4, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048})
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}

	// tenant max_vcpu=4; this VM already counts 2, so growing to 6 exceeds
	// quota regardless of which Hypervisor it lands on.
	if _, err := r.ResizeWithMigration(ctx, tenant, vm.Meta.ID, 6, 2048); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("ResizeWithMigration over quota: got %v, want ErrQuotaExceeded", err)
	}
}
