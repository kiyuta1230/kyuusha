package compute

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestService_RegisterHypervisorUpsertsPreservingReservations(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	h, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil)
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
	h2, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-b", 16, 32768, []string{"FIRECRACKER", "CLOUD_HYPERVISOR"}, nil)
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
	h3, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-b", 16, 32768, []string{"FIRECRACKER"}, nil)
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

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
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

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); !errors.Is(err, ErrHypervisorRevoked) {
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

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("re-Register after un-revoke: %v", err)
	}
}

func TestService_ScheduleVMExcludesUnschedulable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.SetSchedulable(ctx, "hypervisor-1", false); err != nil {
		t.Fatalf("SetSchedulable: %v", err)
	}

	_, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, "")
	if !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("scheduleVM against a cordoned-only Hypervisor: got %v, want ErrUnschedulable", err)
	}

	if _, err := svc.SetSchedulable(ctx, "hypervisor-1", true); err != nil {
		t.Fatalf("SetSchedulable(true): %v", err)
	}
	picked, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, "")
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

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-notready", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := svc.updateHypervisor(ctx, "hypervisor-notready", func(h *Hypervisor) {
		h.Status.Phase = HypervisorPhaseNotReady
	}); err != nil {
		t.Fatalf("force NotReady: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-wrong-driver", "zone-a", 8, 16384, []string{"CLOUD_HYPERVISOR"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-small", "zone-a", 1, 1024, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-good", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	picked, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 2, MemoryMB: 4096}, "")
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

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-a", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-b", "zone-b", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	picked, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, "zone-b")
	if err != nil {
		t.Fatalf("scheduleVM with requiredZone=zone-b: %v", err)
	}
	if picked != "hypervisor-b" {
		t.Fatalf("scheduleVM picked %q, want hypervisor-b", picked)
	}

	// No Hypervisor exists in zone-c: unschedulable despite zone-a/zone-b
	// both having ample spare capacity.
	if _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024}, "zone-c"); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("scheduleVM with requiredZone=zone-c: got %v, want ErrUnschedulable", err)
	}
}

func TestService_ScheduleVMUnschedulableWhenNoCandidateFits(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 1, 1024, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 4, MemoryMB: 8192}, "")
	if err == nil {
		t.Fatal("expected ErrUnschedulable, got nil")
	}
}

func TestService_HeartbeatAndHealthSweep(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Force a stale heartbeat, then sweep: should flip to NotReady.
	if err := svc.updateHypervisor(ctx, "hypervisor-1", func(h *Hypervisor) {
		h.Status.LastHeartbeatAt = time.Now().Add(-1 * time.Hour)
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
