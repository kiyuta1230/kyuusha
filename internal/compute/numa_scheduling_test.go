package compute

import (
	"context"
	"errors"
	"testing"
)

// TestService_ScheduleVMFiltersByNumaCapacity confirms
// scheduleConstraints.NumaPinned excludes a Hypervisor whose NUMA nodes
// don't have enough spare vcpu/memory_mb on any single node, even though
// its aggregate allocatable_vcpu/allocatable_memory_mb would otherwise fit
// -- see docs/architecture.md's NUMA/CPUピニング section. Also confirms
// reserveNumaNode actually reserved against the specific node it picked,
// not just the Hypervisor's aggregate counters.
func TestService_ScheduleVMFiltersByNumaCapacity(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	// Aggregate capacity (8 vcpu/16384MB) would fit a 4-vcpu/8192MB request,
	// but it's split across two small NUMA nodes, neither of which alone
	// has room.
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-split", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, []NumaNode{
		{NodeID: 0, CPUs: []int32{0, 1}, MemoryMB: 8192},
		{NodeID: 1, CPUs: []int32{2, 3}, MemoryMB: 8192},
	}); err != nil {
		t.Fatalf("Register hypervisor-split: %v", err)
	}
	// A single node with enough room for the same request.
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-whole", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, []NumaNode{
		{NodeID: 0, CPUs: []int32{0, 1, 2, 3}, MemoryMB: 16384},
	}); err != nil {
		t.Fatalf("Register hypervisor-whole: %v", err)
	}

	picked, _, numaNode, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 4, MemoryMB: 8192, NumaPinned: true}, scheduleConstraints{NumaPinned: true})
	if err != nil {
		t.Fatalf("scheduleVM: %v", err)
	}
	if picked != "hypervisor-whole" {
		t.Fatalf("scheduleVM picked %q, want hypervisor-whole", picked)
	}
	if numaNode != 0 {
		t.Fatalf("scheduleVM reserved numa node %d, want 0", numaNode)
	}

	h, err := svc.GetHypervisor(ctx, "hypervisor-whole")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if len(h.Status.NumaNodes) != 1 || h.Status.NumaNodes[0].AllocatedVCPU != 4 || h.Status.NumaNodes[0].AllocatedMemoryMB != 8192 {
		t.Fatalf("scheduleVM did not reserve against the node: %+v", h.Status.NumaNodes)
	}
	// The aggregate counters (used by non-pinned scheduling/quota) are also
	// charged, same as any other VM.
	if h.Status.AllocatedVCPU != 4 || h.Status.AllocatedMemoryMB != 8192 {
		t.Fatalf("scheduleVM did not reserve aggregate capacity: %+v", h.Status)
	}
}

func TestService_ScheduleVMUnschedulableWhenNoNumaNodeFits(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, []NumaNode{
		{NodeID: 0, CPUs: []int32{0, 1}, MemoryMB: 4096},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 4, MemoryMB: 4096, NumaPinned: true}, scheduleConstraints{NumaPinned: true}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("scheduleVM over node vcpu capacity: got %v, want ErrUnschedulable", err)
	}
}

// TestService_ScheduleVMNumaPinnedFalseIgnoresNumaNodes confirms a plain
// (non-pinned) VM schedules normally against a Hypervisor whose NUMA nodes
// couldn't individually fit it -- NumaPinned=false must never consult
// NumaNodes at all, only the aggregate counters.
func TestService_ScheduleVMNumaPinnedFalseIgnoresNumaNodes(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-split", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, []NumaNode{
		{NodeID: 0, CPUs: []int32{0, 1}, MemoryMB: 8192},
		{NodeID: 1, CPUs: []int32{2, 3}, MemoryMB: 8192},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	picked, _, numaNode, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 4, MemoryMB: 8192}, scheduleConstraints{})
	if err != nil {
		t.Fatalf("scheduleVM: %v", err)
	}
	if picked != "hypervisor-split" {
		t.Fatalf("scheduleVM picked %q, want hypervisor-split", picked)
	}
	if numaNode != UnpinnedNumaNode {
		t.Fatalf("scheduleVM without numa_pinned reserved node %d, want UnpinnedNumaNode", numaNode)
	}
}

// TestReconciler_DeleteReleasesNumaNodeReservation drives a NUMA-pinned VM
// through Pending -> Scheduled and then Delete, confirming the node's
// allocated_vcpu/allocated_memory_mb return to zero -- the NUMA counterpart
// to the aggregate releaseHypervisorCapacity path every other reconcile
// test already covers.
func TestReconciler_DeleteReleasesNumaNodeReservation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, []NumaNode{
		{NodeID: 0, CPUs: []int32{0, 1, 2, 3}, MemoryMB: 16384},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	vm, err := svc.Create(ctx, "tenant-a", "vm-numa", VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048, NumaPinned: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.reconcile(ctx, *vm)

	scheduled, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if scheduled.Status.Phase != PhaseScheduled {
		t.Fatalf("phase = %s, want Scheduled", scheduled.Status.Phase)
	}
	if scheduled.Status.AllocatedNumaNode != 0 {
		t.Fatalf("AllocatedNumaNode = %d, want 0", scheduled.Status.AllocatedNumaNode)
	}

	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.NumaNodes[0].AllocatedVCPU != 2 || h.Status.NumaNodes[0].AllocatedMemoryMB != 2048 {
		t.Fatalf("node reservation after schedule: %+v", h.Status.NumaNodes[0])
	}

	if err := svc.Delete(ctx, "tenant-a", vm.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	r.releaseIfReserved(ctx, *scheduled)

	h, err = svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.NumaNodes[0].AllocatedVCPU != 0 || h.Status.NumaNodes[0].AllocatedMemoryMB != 0 {
		t.Fatalf("node reservation after Delete: %+v, want zeroed", h.Status.NumaNodes[0])
	}
}

// TestService_ResizeAdjustsNumaNodeReservation confirms Resize's in-place
// path (never re-schedules) still keeps a NUMA-pinned VM's occupied node
// capacity in sync with its new vcpu/memory_mb: a grow that fits the node
// succeeds and updates it, and a grow that doesn't fit is rejected with the
// node's reservation left unchanged (no partial charge).
func TestService_ResizeAdjustsNumaNodeReservation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-1", "zone-a", 16, 32768, []string{"FIRECRACKER"}, nil, nil, []NumaNode{
		{NodeID: 0, CPUs: []int32{0, 1, 2, 3}, MemoryMB: 8192},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	vm, err := svc.Create(ctx, "tenant-a", "vm-numa-resize", VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048, NumaPinned: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.reserveHypervisorCapacity(ctx, "hypervisor-1", vm.Spec.VCPU, vm.Spec.MemoryMB); err != nil {
		t.Fatalf("reserveHypervisorCapacity: %v", err)
	}
	nodeID, err := svc.reserveNumaNode(ctx, "hypervisor-1", vm.Spec.VCPU, vm.Spec.MemoryMB)
	if err != nil {
		t.Fatalf("reserveNumaNode: %v", err)
	}
	vm.Status.Phase = PhaseStopped
	vm.Status.Hypervisor = "hypervisor-1"
	vm.Status.AllocatedNumaNode = nodeID
	if _, err := svc.Update(ctx, vm); err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}

	// Grows to vcpu=4/memory_mb=4096: fits the node (4 cpus, 8192MB total).
	if _, err := svc.Resize(ctx, "tenant-a", vm.Meta.ID, 4, 4096); err != nil {
		t.Fatalf("Resize within node capacity: %v", err)
	}
	h, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.NumaNodes[0].AllocatedVCPU != 4 || h.Status.NumaNodes[0].AllocatedMemoryMB != 4096 {
		t.Fatalf("node reservation after grow: %+v", h.Status.NumaNodes[0])
	}

	// Grows to vcpu=8: the node only has 4 cpus total, so this can never
	// fit even though the Hypervisor's own aggregate allocatable_vcpu=16
	// would allow it.
	if _, err := svc.Resize(ctx, "tenant-a", vm.Meta.ID, 8, 4096); !errors.Is(err, ErrHypervisorCapacityExceeded) {
		t.Fatalf("Resize over node vcpu capacity: got %v, want ErrHypervisorCapacityExceeded", err)
	}
	h, err = svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if h.Status.NumaNodes[0].AllocatedVCPU != 4 || h.Status.NumaNodes[0].AllocatedMemoryMB != 4096 {
		t.Fatalf("node reservation after rejected resize: %+v, want unchanged at vcpu=4 memory_mb=4096", h.Status.NumaNodes[0])
	}
	if h.Status.AllocatedVCPU != 4 || h.Status.AllocatedMemoryMB != 4096 {
		t.Fatalf("aggregate reservation after rejected resize: %+v, want unchanged (rolled back)", h.Status)
	}
}
