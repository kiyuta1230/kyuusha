package compute

import (
	"context"
	"errors"
	"testing"
)

// TestService_ScheduleVMFiltersByStorageConnection confirms
// scheduleConstraints.StorageConnections excludes a Hypervisor that
// doesn't self-report a required connection, even though it otherwise has
// ample capacity -- see docs/specs/volume.md「スケジューリング時のフィルタ
// リング」.
func TestService_ScheduleVMFiltersByStorageConnection(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-no-conn", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register hypervisor-no-conn: %v", err)
	}
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-has-conn", "zone-a", 8, 16384, []string{"FIRECRACKER"},
		[]StorageConnection{{Name: "test-connection"}}); err != nil {
		t.Fatalf("Register hypervisor-has-conn: %v", err)
	}

	picked, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, scheduleConstraints{StorageConnections: []string{"test-connection"}})
	if err != nil {
		t.Fatalf("scheduleVM: %v", err)
	}
	if picked != "hypervisor-has-conn" {
		t.Fatalf("scheduleVM picked %q, want hypervisor-has-conn", picked)
	}
}

func TestService_ScheduleVMUnschedulableWhenNoHypervisorHasRequiredConnection(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, scheduleConstraints{StorageConnections: []string{"missing-connection"}}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("scheduleVM with no matching connection: got %v, want ErrUnschedulable", err)
	}
}

// TestReconciler_PhasePendingSkipsHypervisorMissingStorageConnection drives
// the full Pending -> Scheduled path (reconcile()) for a VM with a Volume,
// confirming it lands on the Hypervisor that declares the required
// connection even though a candidate with more free capacity exists but
// lacks it.
func TestReconciler_PhasePendingSkipsHypervisorMissingStorageConnection(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	// More free capacity, but no storage connection -- must be skipped.
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-roomy-no-conn", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register hypervisor-roomy-no-conn: %v", err)
	}
	// Less free capacity, but has the connection -- must be picked.
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-tight-has-conn", "zone-a", 2, 4096, []string{"FIRECRACKER"},
		[]StorageConnection{{Name: "test-connection"}}); err != nil {
		t.Fatalf("Register hypervisor-tight-has-conn: %v", err)
	}

	vm, err := svc.Create(ctx, tenant, "", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
		Volumes: []VolumeRequest{{VolumeID: "vol-1"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r.reconcile(ctx, *vm)

	got, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != PhaseScheduled {
		t.Fatalf("phase = %q, want Scheduled (conditions: %+v)", got.Status.Phase, got.Status.Conditions)
	}
	if got.Status.Hypervisor != "hypervisor-tight-has-conn" {
		t.Fatalf("Hypervisor = %q, want hypervisor-tight-has-conn (the one with the required storage connection)", got.Status.Hypervisor)
	}
}

// TestReconciler_MigrateSkipsHypervisorMissingStorageConnection confirms
// migrateVM's auto-pick (scheduleMigration) also respects a VM's Volumes'
// required storage connections, not just PhasePending's initial schedule --
// otherwise a migrated VM could land somewhere its Volume can never resolve.
func TestReconciler_MigrateSkipsHypervisorMissingStorageConnection(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512, Volumes: []VolumeRequest{{VolumeID: "vol-1"}}}
	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 16384, spec)
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-no-conn", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register hypervisor-no-conn: %v", err)
	}
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-has-conn", "zone-a", 8, 16384, []string{"FIRECRACKER"},
		[]StorageConnection{{Name: "test-connection"}}); err != nil {
		t.Fatalf("Register hypervisor-has-conn: %v", err)
	}

	migrating, err := svc.Migrate(ctx, tenant, vm.Meta.ID, "")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	got, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Hypervisor != "hypervisor-has-conn" {
		t.Fatalf("Hypervisor = %q, want hypervisor-has-conn (the only candidate with the required storage connection)", got.Status.Hypervisor)
	}
}

func TestReconciler_PhasePendingUnschedulableWhenNoHypervisorHasStorageConnection(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	vm, err := svc.Create(ctx, tenant, "", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
		Volumes: []VolumeRequest{{VolumeID: "vol-1"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r.reconcile(ctx, *vm)

	got, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != PhasePending {
		t.Fatalf("phase = %q, want still Pending", got.Status.Phase)
	}
	found := false
	for _, c := range got.Status.Conditions {
		if c.Type == "Unschedulable" && c.Status == "True" && c.Reason == "InsufficientCapacity" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Conditions = %+v, want an Unschedulable=True/InsufficientCapacity condition", got.Status.Conditions)
	}
}
