package compute

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// waitFor polls done every few milliseconds, failing the test if it never
// becomes true before a short deadline -- needed for anything asserted off
// a fire-and-forget NATS publish (migrateVM's cleanup DeleteCommand to the
// old hypervisor), which lands asynchronously relative to migrateVM itself
// returning.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestService_MigrateRequiresStopped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	vm, err := svc.Create(ctx, "tenant-a", "", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vm.Status.Phase != PhasePending {
		t.Fatalf("phase = %q, want Pending", vm.Status.Phase)
	}

	if _, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "", false); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("Migrate on a Pending vm: got %v, want ErrInvalidPhase", err)
	}
}

func TestService_MigrateRejectsSameHypervisorAsTarget(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)

	if _, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "hypervisor-1", false); !errors.Is(err, ErrValidation) {
		t.Fatalf("Migrate with target == current hypervisor: got %v, want ErrValidation", err)
	}
}

func TestService_MigrateSetsPhaseAndTarget(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "hypervisor-2", false)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if migrating.Status.Phase != PhaseMigrating {
		t.Fatalf("phase = %q, want Migrating", migrating.Status.Phase)
	}
	if migrating.Status.MigrateTarget != "hypervisor-2" {
		t.Fatalf("MigrateTarget = %q, want hypervisor-2", migrating.Status.MigrateTarget)
	}
	// The VM's Hypervisor stays the old one until migrateVM actually
	// reschedules it -- Migrate itself never touches placement.
	if migrating.Status.Hypervisor != "hypervisor-1" {
		t.Fatalf("Hypervisor = %q, want still hypervisor-1 (unchanged until reconcile)", migrating.Status.Hypervisor)
	}
}

// subscribeDeleteCommands records every DeleteCommand published to
// hypervisor's delete subject, mirroring liveops_test.go's
// startFakeHotplugAgent for a different command type.
func subscribeDeleteCommands(t *testing.T, ctx context.Context, js jetstream.JetStream, hypervisor string) *[]DeleteCommand {
	t.Helper()
	var received []DeleteCommand
	stream, err := js.Stream(ctx, "COMPUTE_CMD")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "test-delete-watcher-" + hypervisor,
		FilterSubject: CmdSubjectDelete(hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var cmd DeleteCommand
		if err := json.Unmarshal(msg.Data(), &cmd); err == nil {
			received = append(received, cmd)
		}
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	t.Cleanup(consumeCtx.Stop)
	return &received
}

func TestReconciler_MigrateAutoPicksDifferentHypervisor(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}
	deletesTo1 := subscribeDeleteCommands(t, ctx, r.js, "hypervisor-1")

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "", false)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	got, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != PhaseScheduled {
		t.Fatalf("phase = %q, want Scheduled", got.Status.Phase)
	}
	if got.Status.Hypervisor != "hypervisor-2" {
		t.Fatalf("Hypervisor = %q, want hypervisor-2 (auto-picked, excluding the original)", got.Status.Hypervisor)
	}
	if got.Status.MigrateTarget != "" {
		t.Fatalf("MigrateTarget = %q, want cleared after a successful migrate", got.Status.MigrateTarget)
	}

	old, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-1: %v", err)
	}
	if old.Status.AllocatedVCPU != 0 || old.Status.AllocatedMemoryMB != 0 {
		t.Fatalf("hypervisor-1 allocated = (%d vcpu, %d MB), want fully released", old.Status.AllocatedVCPU, old.Status.AllocatedMemoryMB)
	}
	newH, err := svc.GetHypervisor(ctx, "hypervisor-2")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-2: %v", err)
	}
	if newH.Status.AllocatedVCPU != spec.VCPU || newH.Status.AllocatedMemoryMB != spec.MemoryMB {
		t.Fatalf("hypervisor-2 allocated = (%d vcpu, %d MB), want (%d, %d)", newH.Status.AllocatedVCPU, newH.Status.AllocatedMemoryMB, spec.VCPU, spec.MemoryMB)
	}

	waitFor(t, func() bool { return len(*deletesTo1) == 1 })
	if (*deletesTo1)[0].VMID != vm.Meta.ID {
		t.Fatalf("DeleteCommand to hypervisor-1's vm_id = %q, want %q", (*deletesTo1)[0].VMID, vm.Meta.ID)
	}
}

func TestReconciler_MigrateToExplicitTargetSucceeds(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "hypervisor-2", false)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	got, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Hypervisor != "hypervisor-2" {
		t.Fatalf("Hypervisor = %q, want the explicitly requested hypervisor-2", got.Status.Hypervisor)
	}
}

func TestReconciler_MigrateRejectsTargetLackingCapacity(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 4, MemoryMB: 8192}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)
	// hypervisor-2 exists but is too small for this VM's spec.
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 2, 2048, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "hypervisor-2", false)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	got, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != PhaseMigrating {
		t.Fatalf("phase = %q, want still Migrating (unqualified target rejected)", got.Status.Phase)
	}
	if got.Status.Hypervisor != "hypervisor-1" {
		t.Fatalf("Hypervisor = %q, want unchanged (still hypervisor-1)", got.Status.Hypervisor)
	}
	old, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-1: %v", err)
	}
	if old.Status.AllocatedVCPU != spec.VCPU {
		t.Fatalf("hypervisor-1 allocated vcpu = %d, want still %d (never released on a failed migrate)", old.Status.AllocatedVCPU, spec.VCPU)
	}
}

func TestReconciler_MigrateNoCapacityAnywhereReportsCondition(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}
	// Only one hypervisor exists at all -- auto-pick must exclude it (it's
	// the VM's current one), leaving nothing to schedule onto.
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "", false)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	got, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != PhaseMigrating {
		t.Fatalf("phase = %q, want still Migrating", got.Status.Phase)
	}
	found := false
	for _, c := range got.Status.Conditions {
		if c.Type == "Unmigratable" && c.Status == "True" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Conditions = %+v, want an Unmigratable=True condition", got.Status.Conditions)
	}
}
