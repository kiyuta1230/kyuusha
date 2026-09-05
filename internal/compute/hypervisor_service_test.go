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

	h, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"})
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
	h2, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-b", 16, 32768, []string{"FIRECRACKER", "QEMU"})
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
	h3, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-b", 16, 32768, []string{"FIRECRACKER"})
	if err != nil {
		t.Fatalf("re-Register after cordon: %v", err)
	}
	if h3.Spec.Schedulable {
		t.Fatalf("re-register after SetSchedulable(false) reset it to schedulable")
	}
}

func TestService_ScheduleVMExcludesUnschedulable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.SetSchedulable(ctx, "hypervisor-1", false); err != nil {
		t.Fatalf("SetSchedulable: %v", err)
	}

	_, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024, RecoveryPolicy: RecoveryPolicyNone})
	if !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("scheduleVM against a cordoned-only Hypervisor: got %v, want ErrUnschedulable", err)
	}

	if _, err := svc.SetSchedulable(ctx, "hypervisor-1", true); err != nil {
		t.Fatalf("SetSchedulable(true): %v", err)
	}
	picked, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 1024, RecoveryPolicy: RecoveryPolicyNone})
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

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-notready", "zone-a", 8, 16384, []string{"FIRECRACKER"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := svc.updateHypervisor(ctx, "hypervisor-notready", func(h *Hypervisor) {
		h.Status.Phase = HypervisorPhaseNotReady
	}); err != nil {
		t.Fatalf("force NotReady: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-wrong-driver", "zone-a", 8, 16384, []string{"QEMU"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-small", "zone-a", 1, 1024, []string{"FIRECRACKER"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-good", "zone-a", 8, 16384, []string{"FIRECRACKER"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	picked, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 2, MemoryMB: 4096, RecoveryPolicy: RecoveryPolicyNone})
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

func TestService_ScheduleVMUnschedulableWhenNoCandidateFits(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 1, 1024, []string{"FIRECRACKER"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 4, MemoryMB: 8192, RecoveryPolicy: RecoveryPolicyNone})
	if err == nil {
		t.Fatal("expected ErrUnschedulable, got nil")
	}
}

func TestService_HeartbeatAndHealthSweep(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}); err != nil {
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
