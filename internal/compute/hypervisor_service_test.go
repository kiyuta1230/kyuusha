package compute

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestService_RegisterHypervisorUpsertsPreservingReservations(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	h, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("RegisterHypervisor: %v", err)
	}
	if h.Status.Phase != HypervisorPhaseReady {
		t.Fatalf("phase = %q, want Ready", h.Status.Phase)
	}
	if !h.Spec.Schedulable {
		t.Fatalf("new Hypervisor Schedulable = false, want true (default)")
	}

	if err := svc.reserveHypervisorCapacity(ctx, "hypervisor-1", 2, 4096); err != nil {
		t.Fatalf("reserveHypervisorCapacity: %v", err)
	}

	// Re-register (e.g. agent restart) must refresh capacity/zone but keep
	// the existing reservation intact.
	h2, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-b", 16, 32768, []string{"FIRECRACKER", "CLOUD_HYPERVISOR"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if h2.Meta.ID != h.Meta.ID {
		t.Fatalf("re-register minted a new id: %s vs %s", h2.Meta.ID, h.Meta.ID)
	}
	if h2.Status.Zone != "zone-b" || h2.Status.AllocatableVCPU != 16 {
		t.Fatalf("re-register did not refresh capacity/zone: %+v", h2.Status)
	}
	if h2.Status.AllocatedVCPU != 2 || h2.Status.AllocatedMemoryMB != 4096 {
		t.Fatalf("re-register lost the existing reservation: %+v", h2.Status)
	}

	// A cordon (SetSchedulable(false)) must survive a re-register: an
	// agent restarting mid-maintenance must not silently undo it.
	if _, err := svc.SetSchedulable(ctx, "hypervisor-1", false); err != nil {
		t.Fatalf("SetSchedulable: %v", err)
	}
	h3, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-b", 16, 32768, []string{"FIRECRACKER"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("re-Register after cordon: %v", err)
	}
	if h3.Spec.Schedulable {
		t.Fatalf("re-register after SetSchedulable(false) reset it to schedulable")
	}
}

// TestService_SetRevokedBlocksReRegistrationAndForcesUnschedulable
// exercises docs/specs/hypervisor-bootstrap.md's lightweight per-hypervisor
// identity/revocation: revoking forces Schedulable false in the same call,
// and a later RegisterHypervisor for that same id must be rejected outright
// rather than silently reviving it.
func TestService_SetRevokedBlocksReRegistrationAndForcesUnschedulable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	h, err := svc.SetRevoked(ctx, "hypervisor-1", true)
	if err != nil {
		t.Fatalf("SetRevoked(true): %v", err)
	}
	if !h.Spec.Revoked {
		t.Fatal("Revoked = false after SetRevoked(true)")
	}
	if h.Spec.Schedulable {
		t.Fatal("Schedulable = true after SetRevoked(true), want forced false")
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); !errors.Is(err, ErrHypervisorRevoked) {
		t.Fatalf("re-Register after revoke: got %v, want ErrHypervisorRevoked", err)
	}

	// Un-revoking must not silently restore Schedulable -- that's left as a
	// separate, explicit operator decision.
	h2, err := svc.SetRevoked(ctx, "hypervisor-1", false)
	if err != nil {
		t.Fatalf("SetRevoked(false): %v", err)
	}
	if h2.Spec.Revoked {
		t.Fatal("Revoked = true after SetRevoked(false)")
	}
	if h2.Spec.Schedulable {
		t.Fatal("Schedulable = true after un-revoke, want still false (not auto-restored)")
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("re-Register after un-revoke: %v", err)
	}
}

func TestService_ScheduleVMExcludesUnschedulable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.SetSchedulable(ctx, "hypervisor-1", false); err != nil {
		t.Fatalf("SetSchedulable: %v", err)
	}

	_, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, scheduleConstraints{})
	if !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("scheduleVM against a cordoned-only Hypervisor: got %v, want ErrUnschedulable", err)
	}

	if _, err := svc.SetSchedulable(ctx, "hypervisor-1", true); err != nil {
		t.Fatalf("SetSchedulable(true): %v", err)
	}
	picked, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, scheduleConstraints{})
	if err != nil {
		t.Fatalf("scheduleVM after uncordon: %v", err)
	}
	if picked != "hypervisor-1" {
		t.Fatalf("scheduleVM picked %q, want hypervisor-1", picked)
	}
}

func TestService_ScheduleVMFiltersAndReserves(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-notready", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := svc.updateHypervisor(ctx, "hypervisor-notready", func(h *Hypervisor) error {
		h.Status.Phase = HypervisorPhaseNotReady
		return nil
	}); err != nil {
		t.Fatalf("force NotReady: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-wrong-driver", "zone-a", 8, 16384, []string{"CLOUD_HYPERVISOR"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-small", "zone-a", 1, 1024, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-good", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	picked, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 2, MemoryMB: 4096}, scheduleConstraints{})
	if err != nil {
		t.Fatalf("scheduleVM: %v", err)
	}
	if picked != "hypervisor-good" {
		t.Fatalf("scheduleVM picked %q, want hypervisor-good", picked)
	}

	h, err := svc.GetHypervisor(ctx, "hypervisor-good")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU != 2 || h.Status.AllocatedMemoryMB != 4096 {
		t.Fatalf("scheduleVM did not reserve capacity: %+v", h.Status)
	}

	svc.releaseHypervisorCapacity(ctx, "hypervisor-good", 2, 4096)
	h, err = svc.GetHypervisor(ctx, "hypervisor-good")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU != 0 || h.Status.AllocatedMemoryMB != 0 {
		t.Fatalf("releaseHypervisorCapacity did not release: %+v", h.Status)
	}
}

func TestService_ScheduleVMFiltersByZone(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-a", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-b", "zone-b", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	picked, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, scheduleConstraints{Zone: "zone-b"})
	if err != nil {
		t.Fatalf("scheduleVM with requiredZone=zone-b: %v", err)
	}
	if picked != "hypervisor-b" {
		t.Fatalf("scheduleVM picked %q, want hypervisor-b", picked)
	}

	// No Hypervisor exists in zone-c: unschedulable despite zone-a/zone-b
	// both having ample spare capacity.
	if _, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, scheduleConstraints{Zone: "zone-c"}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("scheduleVM with requiredZone=zone-c: got %v, want ErrUnschedulable", err)
	}
}

func TestService_ScheduleVMUnschedulableWhenNoCandidateFits(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 1, 1024, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 4, MemoryMB: 8192}, scheduleConstraints{})
	if err == nil {
		t.Fatal("expected ErrUnschedulable, got nil")
	}
}

// TestService_ReserveHypervisorCapacityRaceNeverOversubscribes fires more
// concurrent reservations at one Hypervisor than its capacity allows,
// against a real embedded etcd (resourcetest.Client) so updateHypervisor's
// optimistic-concurrency retries are exercising genuine CAS conflicts, not
// a fake store that might not reproduce them. Before reserveHypervisorCapacity
// re-checked fit against the freshly-fetched Hypervisor on every retry (see
// its doc comment), every one of these could have raced past
// filterSchedulable's earlier, now-stale check and all succeeded, pushing
// AllocatedVCPU past AllocatableVCPU.
func TestService_ReserveHypervisorCapacityRaceNeverOversubscribes(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	const allocatableVCPU = 6
	const perReservation = 2
	const attempts = 6 // demands 12 vCPU total against 6 available: at most 3 can win

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", allocatableVCPU, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = svc.reserveHypervisorCapacity(ctx, "hypervisor-1", perReservation, 1024)
		}(i)
	}
	wg.Wait()

	var succeeded int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrUnschedulable):
			// Correctly lost the race: fit no longer available by the time
			// this attempt's write actually landed.
		default:
			t.Fatalf("reserveHypervisorCapacity: unexpected error: %v", err)
		}
	}

	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU > allocatableVCPU {
		t.Fatalf("AllocatedVCPU = %d, want <= %d (capacity was oversubscribed)", h.Status.AllocatedVCPU, allocatableVCPU)
	}
	if want := int32(succeeded) * perReservation; h.Status.AllocatedVCPU != want {
		t.Fatalf("AllocatedVCPU = %d, want %d (%d reservations succeeded)", h.Status.AllocatedVCPU, want, succeeded)
	}
	if succeeded == 0 || succeeded == attempts {
		t.Fatalf("succeeded = %d/%d, want somewhere strictly in between to actually exercise the race (adjust attempts/capacity if this is flaky)", succeeded, attempts)
	}
}

// TestService_ResizeRejectsWhenHypervisorLacksCapacity confirms a grow
// Resize that doesn't fit the VM's already-assigned Hypervisor is rejected
// outright (no cross-hypervisor migration -- see docs/specs/vm-scheduling.md),
// and that rejection leaves the Hypervisor's reservation untouched (no
// partial charge from the capacity check itself).
func TestService_ResizeRejectsWhenHypervisorLacksCapacity(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 4, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 4096})

	// Only 2 more vCPU free (4 allocatable - 2 allocated); asking for 6 more (2->8) doesn't fit.
	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 8, 4096); !errors.Is(err, ErrHypervisorCapacityExceeded) {
		t.Fatalf("over hypervisor capacity: got %v, want ErrHypervisorCapacityExceeded", err)
	}

	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU != 2 || h.Status.AllocatedMemoryMB != 4096 {
		t.Fatalf("Hypervisor allocation changed after a rejected Resize: %+v, want unchanged vcpu=2 memory_mb=4096", h.Status)
	}
	stored, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Spec.VCPU != 2 {
		t.Fatalf("stored Spec.VCPU changed after a rejected Resize: %d, want unchanged 2", stored.Spec.VCPU)
	}
}

// TestService_ResizeGrowsHypervisorReservation confirms a successful grow
// increases AllocatedVCPU/AllocatedMemoryMB by exactly the delta.
func TestService_ResizeGrowsHypervisorReservation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 16384, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048})

	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 6, 5120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU != 6 || h.Status.AllocatedMemoryMB != 5120 {
		t.Fatalf("AllocatedVCPU/MemoryMB = %d/%d, want 6/5120", h.Status.AllocatedVCPU, h.Status.AllocatedMemoryMB)
	}
}

// TestService_ResizeShrinkReleasesHypervisorReservation confirms a
// successful shrink decreases AllocatedVCPU/AllocatedMemoryMB by exactly
// the delta, rather than leaking the old, larger reservation.
func TestService_ResizeShrinkReleasesHypervisorReservation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 16384, VirtualMachineSpec{ImageID: "img-abc", VCPU: 6, MemoryMB: 6144})

	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 2, 2048); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU != 2 || h.Status.AllocatedMemoryMB != 2048 {
		t.Fatalf("AllocatedVCPU/MemoryMB = %d/%d, want 2/2048 (shrink must free capacity, not leak it)", h.Status.AllocatedVCPU, h.Status.AllocatedMemoryMB)
	}
}

// TestService_ResizeCapacityRaceNeverOversubscribes mirrors
// TestService_ReserveHypervisorCapacityRaceNeverOversubscribes for
// resizeHypervisorCapacity: fires more concurrent grow-deltas at one
// Hypervisor than its capacity allows, calling resizeHypervisorCapacity
// directly (as Resize's own global usageMu would otherwise fully serialize
// every Resize call and never exercise this race -- the real concurrency
// this needs to be safe against is a concurrent Resize racing a concurrent
// Create/scheduleVM onto the same Hypervisor, both of which bottom out in
// updateHypervisor's retry-on-conflict CAS loop, independent of usageMu).
func TestService_ResizeCapacityRaceNeverOversubscribes(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	const allocatableVCPU = 6
	const perDelta = 2
	const attempts = 6 // demands 12 vCPU total against 6 available: at most 3 can win

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", allocatableVCPU, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = svc.resizeHypervisorCapacity(ctx, "hypervisor-1", perDelta, 1024)
		}(i)
	}
	wg.Wait()

	var succeeded int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrHypervisorCapacityExceeded):
			// Correctly lost the race: fit no longer available by the time
			// this attempt's write actually landed.
		default:
			t.Fatalf("resizeHypervisorCapacity: unexpected error: %v", err)
		}
	}

	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.AllocatedVCPU > allocatableVCPU {
		t.Fatalf("AllocatedVCPU = %d, want <= %d (capacity was oversubscribed)", h.Status.AllocatedVCPU, allocatableVCPU)
	}
	if want := int32(succeeded) * perDelta; h.Status.AllocatedVCPU != want {
		t.Fatalf("AllocatedVCPU = %d, want %d (%d resizes succeeded)", h.Status.AllocatedVCPU, want, succeeded)
	}
	if succeeded == 0 || succeeded == attempts {
		t.Fatalf("succeeded = %d/%d, want somewhere strictly in between to actually exercise the race (adjust attempts/capacity if this is flaky)", succeeded, attempts)
	}
}

func TestService_HeartbeatAndHealthSweep(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Force a stale heartbeat, then sweep: should flip to NotReady.
	if err := svc.updateHypervisor(ctx, "hypervisor-1", func(h *Hypervisor) error {
		h.Status.LastHeartbeatAt = time.Now().Add(-1 * time.Hour)
		return nil
	}); err != nil {
		t.Fatalf("force stale heartbeat: %v", err)
	}
	svc.sweepHypervisorHealth(ctx)
	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.Phase != HypervisorPhaseNotReady {
		t.Fatalf("phase after sweep = %q, want NotReady", h.Status.Phase)
	}

	// A fresh Heartbeat revives it.
	if err := svc.Heartbeat(ctx, "hypervisor-1", time.Now()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	h, err = svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.Phase != HypervisorPhaseReady {
		t.Fatalf("phase after heartbeat = %q, want Ready", h.Status.Phase)
	}
}
