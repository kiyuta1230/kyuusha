package compute

import (
	"context"
	"errors"
	"testing"
	"time"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func newTestService(t *testing.T, ctx context.Context) *Service {
	t.Helper()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// playground: drives the Service the way a real client (KaaS controller,
// CLI, ...) would: Create, observe it over Watch, Update with optimistic
// concurrency, Delete, and confirm Watch reports all of it. Kept as a normal
// test so it runs (and stays honest) under `go test ./...`.
func TestPlayground_VirtualMachineLifecycleOverWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	events, err := svc.Watch(ctx, tenant, 0, "")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	m, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{
		ImageID:  "img-abc",
		VCPU:     2,
		MemoryMB: 4096,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.Status.Phase != PhasePending {
		t.Fatalf("new VirtualMachine phase = %q, want %q", m.Status.Phase, PhasePending)
	}
	if m.Spec.DriverHint != VmmDriverFirecracker {
		t.Fatalf("DriverHint = %q, want default FIRECRACKER", m.Spec.DriverHint)
	}

	// Idempotent re-Create with the same name must not mint a new ID.
	again, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{ImageID: "img-abc"})
	if err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	if again.Meta.ID != m.Meta.ID {
		t.Fatalf("idempotent Create minted a new ID: %s vs %s", again.Meta.ID, m.Meta.ID)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("RegisterHypervisor: %v", err)
	}

	// Simulate the scheduler advancing the phase, with optimistic concurrency.
	m.Status.Phase = PhaseScheduled
	m.Status.Hypervisor = "hypervisor-1"
	updated, err := svc.Update(ctx, m)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := svc.Update(ctx, m); err != ErrConflict {
		t.Fatalf("stale Update: got %v, want ErrConflict", err)
	}

	// Drive Scheduled -> Running (simulating reconcile()'s real
	// provisioning), then a Stop -> Resize -> Start cycle: the same path a
	// real client would use to change a VM's vcpu/memory_mb (see
	// Service.Resize -- cold resize only, Stopped -> Stopped).
	updated.Status.Phase = PhaseRunning
	running, err := svc.Update(ctx, updated)
	if err != nil {
		t.Fatalf("Update to Running: %v", err)
	}

	stopping, err := svc.Stop(ctx, tenant, running.Meta.ID, false)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopping.Status.Phase = PhaseStopped // simulate reconcile() completing the teardown
	stopped, err := svc.Update(ctx, stopping)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}

	resized, err := svc.Resize(ctx, tenant, stopped.Meta.ID, 4, 8192)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if resized.Spec.VCPU != 4 || resized.Spec.MemoryMB != 8192 {
		t.Fatalf("Spec after Resize = %+v, want vcpu=4 memory_mb=8192", resized.Spec)
	}

	started, err := svc.Start(ctx, tenant, resized.Meta.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Confirms provisionAndPublish would boot with the resized values: Start
	// itself never touches Spec, so this is really asserting Resize's write
	// stuck and Start left it alone.
	if started.Spec.VCPU != 4 || started.Spec.MemoryMB != 8192 {
		t.Fatalf("Spec after Start = %+v, want the resized vcpu=4 memory_mb=8192 to have survived", started.Spec)
	}

	if err := svc.Delete(ctx, tenant, started.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Added, then some number of Modified events (Scheduled, Running,
	// Stopping, Stopped, the Resize, Starting), then Deleted.
	select {
	case e := <-events:
		if e.Type != EventAdded {
			t.Fatalf("event type = %s, want %s", e.Type, EventAdded)
		}
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s event", EventAdded)
	}
	var modifiedCount int
drain:
	for {
		select {
		case e := <-events:
			switch e.Type {
			case EventModified:
				modifiedCount++
			case EventDeleted:
				break drain
			default:
				t.Fatalf("unexpected event type %s", e.Type)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s event", EventDeleted)
		}
	}
	if modifiedCount == 0 {
		t.Fatalf("no Modified events observed before Deleted")
	}

	// A fresh Watch resumed from the last known resource_version should see
	// nothing new (no relist error, empty backlog) since we're caught up.
	lastRV := started.Meta.ResourceVersion + 1 // +1 for the Delete event
	resumed, err := svc.Watch(ctx, tenant, lastRV, "")
	if err != nil {
		t.Fatalf("resumed Watch: %v", err)
	}
	select {
	case e := <-resumed:
		t.Fatalf("unexpected event on resumed watch: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestService_GetIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	m, err := svc.Create(ctx, "tenant-a", "", VirtualMachineSpec{ImageID: "img-abc"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(ctx, "tenant-b", m.Meta.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant Get: got %v, want ErrNotFound", err)
	}
}

// TestService_DeleteWithFinalizerBlocksUntilCleared exercises
// docs/architecture.md "Finalizer": an external controller (e.g. one
// checking an external network ACL before letting a VM's IP be reused, see
// the discussion that motivated this) can hold a VM open past Delete by
// adding its own name to Meta.Finalizers, and the VM only actually
// disappears once that's removed via Update.
func TestService_DeleteWithFinalizerBlocksUntilCleared(t *testing.T) {
	ctx := context.Background()
	// A tight max_vms=1 quota (rather than newTestService's unlimited one)
	// is what makes the "tenant_usage was already decremented at Delete-
	// call time" assertion below meaningful.
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu: 8, MaxMemoryMb: 8192, MaxVms: 1, MaxVcpuPerVm: 8, MaxMemoryMbPerVm: 8192,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	vm.Meta.Finalizers = []resource.Finalizer{{Name: "acme.corp/network-acl-cleanup"}}
	updated, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to add finalizer: %v", err)
	}

	if err := svc.Delete(ctx, tenant, updated.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	pending, err := svc.Get(ctx, tenant, updated.Meta.ID)
	if err != nil {
		t.Fatalf("Get after Delete (finalizer pending): %v", err)
	}
	if pending.Meta.DeletedAt == nil {
		t.Fatal("DeletedAt was not set")
	}
	if pending.Status.Phase != PhaseDeleting {
		t.Fatalf("Phase = %q, want Deleting", pending.Status.Phase)
	}

	// tenant_usage was already decremented at Delete-call time (see
	// Service.Delete's doc comment): a second VM the same size fits even
	// though the first, finalizer-blocked one still physically exists.
	if _, err := svc.Create(ctx, tenant, "web-2", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
	}); err != nil {
		t.Fatalf("Create after Delete-with-finalizer freed quota: %v", err)
	}

	// A repeated Delete call must not double-decrement tenant_usage or
	// error out.
	if err := svc.Delete(ctx, tenant, updated.Meta.ID); err != nil {
		t.Fatalf("second Delete while finalizer still pending: %v", err)
	}

	pending.Meta.Finalizers = nil
	if _, err := svc.Update(ctx, pending); err != nil {
		t.Fatalf("Update clearing the last finalizer: %v", err)
	}
	if _, err := svc.Get(ctx, tenant, updated.Meta.ID); err != ErrNotFound {
		t.Fatalf("Get after clearing the last finalizer: got %v, want ErrNotFound", err)
	}
}

// TestService_FinalizerOwnership exercises checkFinalizerMutation through
// Service.Update: a finalizer's AddedBy is stamped server-side from the
// caller's propagated identity (internal/authn.ContextWithPropagatedCallerForTest
// simulates what api-gateway's client interceptor would have attached) and
// only that same caller, or an admin, may later remove it.
func TestService_FinalizerOwnership(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	aliceCtx := authn.ContextWithPropagatedCallerForTest(ctx, "alice", false)
	vm.Meta.Finalizers = []resource.Finalizer{{Name: "acme.corp/network-acl-cleanup", AddedBy: "someone-else-entirely"}}
	added, err := svc.Update(aliceCtx, vm)
	if err != nil {
		t.Fatalf("Update to add finalizer as alice: %v", err)
	}
	if got := added.Meta.Finalizers[0].AddedBy; got != "alice" {
		t.Fatalf("AddedBy = %q, want %q (client-supplied value must be ignored and server-stamped)", got, "alice")
	}

	bobCtx := authn.ContextWithPropagatedCallerForTest(ctx, "bob", false)
	rejected := added
	rejected.Meta.Finalizers = nil
	if _, err := svc.Update(bobCtx, rejected); !errors.Is(err, ErrValidation) {
		t.Fatalf("Update removing alice's finalizer as bob: got %v, want ErrValidation", err)
	}
	// The rejected removal must not have taken effect.
	stillThere, err := svc.Get(ctx, tenant, added.Meta.ID)
	if err != nil {
		t.Fatalf("Get after rejected removal: %v", err)
	}
	if len(stillThere.Meta.Finalizers) != 1 {
		t.Fatalf("Finalizers = %v, want alice's entry to survive bob's rejected removal", stillThere.Meta.Finalizers)
	}

	adminCtx := authn.ContextWithPropagatedCallerForTest(ctx, "carol", true)
	byAdmin := stillThere
	byAdmin.Meta.Finalizers = nil
	if _, err := svc.Update(adminCtx, byAdmin); err != nil {
		t.Fatalf("Update removing alice's finalizer as admin: %v", err)
	}
	afterAdmin, err := svc.Get(ctx, tenant, added.Meta.ID)
	if err != nil {
		t.Fatalf("Get after admin removal: %v", err)
	}
	if len(afterAdmin.Meta.Finalizers) != 0 {
		t.Fatalf("Finalizers = %v, want empty after admin removal", afterAdmin.Meta.Finalizers)
	}

	// alice can add another one and remove it herself.
	afterAdmin.Meta.Finalizers = []resource.Finalizer{{Name: "acme.corp/again"}}
	reAdded, err := svc.Update(aliceCtx, afterAdmin)
	if err != nil {
		t.Fatalf("Update to re-add finalizer as alice: %v", err)
	}
	reAdded.Meta.Finalizers = nil
	if _, err := svc.Update(aliceCtx, reAdded); err != nil {
		t.Fatalf("Update removing alice's own finalizer as alice: %v", err)
	}
}

// TestService_StopRequiresRunning exercises the VM lifecycle's phase
// guard (docs/architecture.md's VM lifecycle: Stop only makes sense on a
// VM compute-agent might actually have a live process for) and confirms a
// valid Stop moves Running -> Stopping and stashes force on Status
// (consumed by reconciler.go's PhaseStopping case, see nats.go's
// StopCommand).
func TestService_StopRequiresRunning(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Stop(ctx, tenant, vm.Meta.ID, false); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("Stop on Pending VM: got %v, want ErrInvalidPhase", err)
	}

	vm.Status.Phase = PhaseRunning
	vm.Status.Hypervisor = "hypervisor-1"
	running, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Running: %v", err)
	}

	stopped, err := svc.Stop(ctx, tenant, running.Meta.ID, true)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Status.Phase != PhaseStopping {
		t.Fatalf("Phase = %q, want Stopping", stopped.Status.Phase)
	}
	if !stopped.Status.StopForce {
		t.Fatal("StopForce = false, want true (Stop was called with force=true)")
	}

	if _, err := svc.Stop(ctx, tenant, stopped.Meta.ID, false); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("Stop on already-Stopping VM: got %v, want ErrInvalidPhase", err)
	}
}

// TestService_StartRequiresStopped mirrors TestService_StopRequiresRunning
// for the Stopped -> Starting transition.
func TestService_StartRequiresStopped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Start(ctx, tenant, vm.Meta.ID); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("Start on Pending VM: got %v, want ErrInvalidPhase", err)
	}

	vm.Status.Phase = PhaseStopped
	vm.Status.Hypervisor = "hypervisor-1"
	vm.Status.InterfaceRefs = []string{"iface-1"}
	stoppedVM, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}

	started, err := svc.Start(ctx, tenant, stoppedVM.Meta.ID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status.Phase != PhaseStarting {
		t.Fatalf("Phase = %q, want Starting", started.Status.Phase)
	}
	// Start must not disturb the VM's existing Hypervisor assignment or
	// InterfaceRefs -- see Service.Start's doc comment: reconcile() reuses
	// them exactly as-is via provisionAndPublish, nothing here re-derives them.
	if started.Status.Hypervisor != "hypervisor-1" {
		t.Fatalf("Hypervisor = %q, want unchanged hypervisor-1", started.Status.Hypervisor)
	}

	if _, err := svc.Start(ctx, tenant, started.Meta.ID); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("Start on already-Starting VM: got %v, want ErrInvalidPhase", err)
	}
}

// stoppedVMWithHypervisor creates a VM, registers a Hypervisor with the
// given capacity, forces the VM to Stopped + pinned to that Hypervisor (as
// if it had already been scheduled and stopped -- Resize tests don't drive
// the reconciler), and reserves the VM's own spec against that Hypervisor
// (mirroring what real scheduling would already have done), so a resize's
// capacity delta check has a realistic baseline to work against.
func stoppedVMWithHypervisor(t *testing.T, ctx context.Context, svc *Service, tenant, hypervisorID string, allocatableVCPU int32, allocatableMemoryMB int64, spec VirtualMachineSpec) VirtualMachine {
	t.Helper()
	if _, err := svc.RegisterHypervisor(ctx, hypervisorID, "zone-a", allocatableVCPU, allocatableMemoryMB, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("RegisterHypervisor: %v", err)
	}
	vm, err := svc.Create(ctx, tenant, "", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.reserveHypervisorCapacity(ctx, hypervisorID, spec.VCPU, spec.MemoryMB); err != nil {
		t.Fatalf("reserveHypervisorCapacity: %v", err)
	}
	vm.Status.Phase = PhaseStopped
	vm.Status.Hypervisor = hypervisorID
	stopped, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}
	return *stopped
}

// TestService_ResizeRequiresStopped mirrors TestService_StopRequiresRunning/
// TestService_StartRequiresStopped for Resize's own phase gate.
func TestService_ResizeRequiresStopped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 2, 1024); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("Resize on Pending VM: got %v, want ErrInvalidPhase", err)
	}

	vm.Status.Phase = PhaseRunning
	vm.Status.Hypervisor = "hypervisor-1"
	running, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Running: %v", err)
	}
	if _, err := svc.Resize(ctx, tenant, running.Meta.ID, 2, 1024); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("Resize on Running VM: got %v, want ErrInvalidPhase", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("RegisterHypervisor: %v", err)
	}
	running.Status.Phase = PhaseStopped
	stoppedVM, err := svc.Update(ctx, running)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}

	resized, err := svc.Resize(ctx, tenant, stoppedVM.Meta.ID, 2, 1024)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if resized.Spec.VCPU != 2 || resized.Spec.MemoryMB != 1024 {
		t.Fatalf("Spec = %+v, want vcpu=2 memory_mb=1024", resized.Spec)
	}
	if resized.Status.Phase != PhaseStopped {
		t.Fatalf("Phase = %q, want unchanged Stopped", resized.Status.Phase)
	}
}

func TestService_ResizeRejectsNonPositiveValues(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 16384, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})

	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 0, 1024); !errors.Is(err, ErrValidation) {
		t.Fatalf("vcpu=0: got %v, want ErrValidation", err)
	}
	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 2, -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("memory_mb=-1: got %v, want ErrValidation", err)
	}
}

func TestService_ResizeNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})

	if _, err := svc.Resize(ctx, "tenant-b", vm.Meta.ID, 2, 1024); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Resize: got %v, want ErrNotFound", err)
	}
}

// TestService_ResizeSameSizeIsNoop confirms a resize to the VM's current
// size returns successfully without bumping resource_version or otherwise
// touching stored state -- deliberately not routed through the
// quota/capacity/store.Update machinery at all (see Service.Resize's
// no-op short-circuit).
func TestService_ResizeSameSizeIsNoop(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 16384, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048})

	out, err := svc.Resize(ctx, tenant, vm.Meta.ID, 2, 2048)
	if err != nil {
		t.Fatalf("Resize to same size: %v", err)
	}
	if out.Meta.ResourceVersion != vm.Meta.ResourceVersion {
		t.Fatalf("ResourceVersion changed on a same-size Resize: %d -> %d", vm.Meta.ResourceVersion, out.Meta.ResourceVersion)
	}

	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU != 2 || h.Status.AllocatedMemoryMB != 2048 {
		t.Fatalf("Hypervisor allocation changed on a same-size Resize: %+v", h.Status)
	}
}

// TestService_ResizeUpdatesTenantUsage confirms tenant_usage tracks the
// vcpu/memory_mb delta (both growing and shrinking) without touching
// VMCount, mirroring Create/Delete's own usage bookkeeping.
func TestService_ResizeUpdatesTenantUsage(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 16384, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048})

	before := svc.usage[tenant]
	if before.VCPU != 2 || before.MemoryMB != 2048 || before.VMCount != 1 {
		t.Fatalf("usage before Resize = %+v, want vcpu=2 memory_mb=2048 vm_count=1", before)
	}

	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 4, 4096); err != nil {
		t.Fatalf("grow Resize: %v", err)
	}
	afterGrow := svc.usage[tenant]
	if afterGrow.VCPU != 4 || afterGrow.MemoryMB != 4096 || afterGrow.VMCount != 1 {
		t.Fatalf("usage after grow = %+v, want vcpu=4 memory_mb=4096 vm_count=1 (unchanged)", afterGrow)
	}

	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 1, 512); err != nil {
		t.Fatalf("shrink Resize: %v", err)
	}
	afterShrink := svc.usage[tenant]
	if afterShrink.VCPU != 1 || afterShrink.MemoryMB != 512 || afterShrink.VMCount != 1 {
		t.Fatalf("usage after shrink = %+v, want vcpu=1 memory_mb=512 vm_count=1 (unchanged)", afterShrink)
	}
}

// TestService_WatchFilterByFinalizerName exercises the finalizer_name Watch
// filter (docs/architecture.md "Finalizer" 's "外部システムが大量にWatch
// する" concern): an external controller that only cares about VMs it has
// placed its own finalizer on should be able to Watch just those, not every
// VM in the tenant.
func TestService_WatchFilterByFinalizerName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vmA, err := svc.Create(ctx, tenant, "vm-a", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
	})
	if err != nil {
		t.Fatalf("Create vm-a: %v", err)
	}
	vmB, err := svc.Create(ctx, tenant, "vm-b", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512,
	})
	if err != nil {
		t.Fatalf("Create vm-b: %v", err)
	}

	events, err := svc.Watch(ctx, tenant, 0, "acme.corp/only-vm-a")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	vmA.Meta.Finalizers = []resource.Finalizer{{Name: "acme.corp/only-vm-a"}}
	if _, err := svc.Update(ctx, vmA); err != nil {
		t.Fatalf("Update vm-a to add finalizer: %v", err)
	}
	vmB.Meta.Finalizers = []resource.Finalizer{{Name: "acme.corp/only-vm-b"}}
	if _, err := svc.Update(ctx, vmB); err != nil {
		t.Fatalf("Update vm-b to add finalizer: %v", err)
	}

	select {
	case e := <-events:
		if e.Object.Meta.ID != vmA.Meta.ID {
			t.Fatalf("filtered Watch surfaced vm %s, want only vm-a (%s)", e.Object.Meta.ID, vmA.Meta.ID)
		}
		if e.Type != EventModified {
			t.Fatalf("event type = %s, want Modified", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vm-a's finalizer-add event")
	}

	// vm-b's update (a different finalizer name) must never reach this
	// filtered watch.
	select {
	case e := <-events:
		t.Fatalf("unexpected event for a non-matching VM: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
}
