package compute

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// startTestNATS mirrors playground/compute_test.go's startNATS -- an
// embedded, ephemeral JetStream server, duplicated here rather than
// imported since playground imports this package (an import the other way
// would cycle).
func startTestNATS(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("start embedded nats: %v", err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats server never became ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect to embedded nats: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := EnsureStreams(context.Background(), js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	return nc, js
}

// newTestReconciler wires a Reconciler against an embedded NATS/JetStream
// server and svc -- Run() is never called (same as cmd/compute's own
// construction), only the direct methods in liveops.go/console.go are
// exercised.
func newTestReconciler(t *testing.T, ctx context.Context, svc *Service) *Reconciler {
	t.Helper()
	nc, js := startTestNATS(t)
	return NewReconciler(svc, nc, js)
}

// fakeHotplugAgent stands in for compute-agent's handleHotplug (internal/
// compute-agent/hotplug.go): subscribes to hypervisor's CmdSubjectHotplug,
// and replies to every command it receives with result (or, if
// resultForOp is set, a per-Op override -- used to make only one op in a
// multi-step test fail).
type fakeHotplugAgent struct {
	nc           *nats.Conn
	js           jetstream.JetStream
	hypervisor   string
	result       HotplugResult
	resultForOp  map[HotplugOp]HotplugResult
	receivedCmds []HotplugCommand
}

func startFakeHotplugAgent(t *testing.T, ctx context.Context, nc *nats.Conn, js jetstream.JetStream, hypervisor string, defaultResult HotplugResult) *fakeHotplugAgent {
	t.Helper()
	a := &fakeHotplugAgent{nc: nc, js: js, hypervisor: hypervisor, result: defaultResult}

	stream, err := js.Stream(ctx, "COMPUTE_CMD")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "test-fake-agent-" + hypervisor,
		FilterSubject: CmdSubjectHotplug(hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var cmd HotplugCommand
		if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
			return
		}
		a.receivedCmds = append(a.receivedCmds, cmd)
		res := a.result
		if override, ok := a.resultForOp[cmd.Op]; ok {
			res = override
		}
		payload, _ := json.Marshal(res)
		_ = a.nc.Publish(cmd.ReplySubject, payload)
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	t.Cleanup(consumeCtx.Stop)
	return a
}

// runningVMWithHypervisor mirrors stoppedVMWithHypervisor (service_test.go)
// but for the live-op eligible state: Running, pinned to a CLOUD_HYPERVISOR
// Hypervisor, with the VM's own spec already reserved against it.
func runningVMWithHypervisor(t *testing.T, ctx context.Context, svc *Service, tenant, hypervisorID string, allocatableVCPU int32, allocatableMemoryMB int64, spec VirtualMachineSpec) *VirtualMachine {
	t.Helper()
	spec.DriverHint = VmmDriverCloudHypervisor
	if _, err := svc.RegisterHypervisor(ctx, hypervisorID, "zone-a", allocatableVCPU, allocatableMemoryMB, []string{"CLOUD_HYPERVISOR"}, nil); err != nil {
		t.Fatalf("RegisterHypervisor: %v", err)
	}
	vm, err := svc.Create(ctx, tenant, "", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.reserveHypervisorCapacity(ctx, hypervisorID, spec.VCPU, spec.MemoryMB); err != nil {
		t.Fatalf("reserveHypervisorCapacity: %v", err)
	}
	vm.Status.Phase = PhaseRunning
	vm.Status.Hypervisor = hypervisorID
	running, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Running: %v", err)
	}
	return running
}

func TestReconciler_LiveResizeRequiresRunningCloudHypervisor(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	// Stopped VM: still cold-only.
	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512, DriverHint: VmmDriverCloudHypervisor})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	vm.Status.Phase = PhaseStopped
	vm.Status.Hypervisor = "hypervisor-1"
	stopped, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}
	if _, err := r.LiveResize(ctx, tenant, stopped.Meta.ID, 2, 1024); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("LiveResize on Stopped VM: got %v, want ErrInvalidPhase", err)
	}

	// Running FIRECRACKER VM: live resize is CLOUD_HYPERVISOR-only.
	fcVM, err := svc.Create(ctx, tenant, "web-2", VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 512, DriverHint: VmmDriverFirecracker})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fcVM.Status.Phase = PhaseRunning
	fcVM.Status.Hypervisor = "hypervisor-1"
	runningFC, err := svc.Update(ctx, fcVM)
	if err != nil {
		t.Fatalf("Update to Running: %v", err)
	}
	if _, err := r.LiveResize(ctx, tenant, runningFC.Meta.ID, 4, 1024); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("LiveResize on Running+FIRECRACKER VM: got %v, want ErrInvalidPhase", err)
	}
}

func TestReconciler_LiveResizeSameSizeIsNoop(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := runningVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})

	out, err := r.LiveResize(ctx, tenant, vm.Meta.ID, 2, 1024)
	if err != nil {
		t.Fatalf("LiveResize same size: %v", err)
	}
	if out.Spec.VCPU != 2 || out.Spec.MemoryMB != 1024 {
		t.Fatalf("Spec changed on a same-size resize: %+v", out.Spec)
	}
}

func TestReconciler_LiveResizeSuccess(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := runningVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})
	startFakeHotplugAgent(t, ctx, r.nc, r.js, "hypervisor-1", HotplugResult{Success: true})

	out, err := r.LiveResize(ctx, tenant, vm.Meta.ID, 4, 2048)
	if err != nil {
		t.Fatalf("LiveResize: %v", err)
	}
	if out.Spec.VCPU != 4 || out.Spec.MemoryMB != 2048 {
		t.Fatalf("Spec after resize = %+v, want vcpu=4 memory_mb=2048", out.Spec)
	}

	hv, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if hv.Status.AllocatedVCPU != 4 || hv.Status.AllocatedMemoryMB != 2048 {
		t.Fatalf("Hypervisor allocation after resize = vcpu=%d memory_mb=%d, want 4/2048", hv.Status.AllocatedVCPU, hv.Status.AllocatedMemoryMB)
	}
}

func TestReconciler_LiveResizeRollsBackCapacityOnHotplugFailure(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := runningVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})
	startFakeHotplugAgent(t, ctx, r.nc, r.js, "hypervisor-1", HotplugResult{Success: false, Error: "cloud-hypervisor: resize past max= ceiling"})

	if _, err := r.LiveResize(ctx, tenant, vm.Meta.ID, 4, 2048); err == nil {
		t.Fatal("expected LiveResize to fail when compute-agent reports failure")
	}

	hv, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor: %v", err)
	}
	if hv.Status.AllocatedVCPU != 2 || hv.Status.AllocatedMemoryMB != 1024 {
		t.Fatalf("capacity not rolled back: vcpu=%d memory_mb=%d, want the original 2/1024", hv.Status.AllocatedVCPU, hv.Status.AllocatedMemoryMB)
	}

	got, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.VCPU != 2 || got.Spec.MemoryMB != 1024 {
		t.Fatalf("spec changed despite hotplug failure: %+v", got.Spec)
	}
}

func TestReconciler_LiveAttachVolumeRequiresRunningCloudHypervisor(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512, DriverHint: VmmDriverCloudHypervisor})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	vm.Status.Phase = PhaseStopped
	vm.Status.Hypervisor = "hypervisor-1"
	stopped, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}
	if _, err := r.LiveAttachVolume(ctx, tenant, stopped.Meta.ID, "vol-1", ""); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("LiveAttachVolume on Stopped VM: got %v, want ErrInvalidPhase", err)
	}
}

func TestReconciler_LiveAttachVolumeSuccess(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := runningVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})
	agent := startFakeHotplugAgent(t, ctx, r.nc, r.js, "hypervisor-1", HotplugResult{Success: true})

	out, err := r.LiveAttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", "")
	if err != nil {
		t.Fatalf("LiveAttachVolume: %v", err)
	}
	if len(out.Spec.Volumes) != 1 || out.Spec.Volumes[0].VolumeID != "vol-1" {
		t.Fatalf("Spec.Volumes = %+v, want one entry for vol-1", out.Spec.Volumes)
	}
	if len(out.Status.VolumeAttachmentRefs) != 1 {
		t.Fatalf("Status.VolumeAttachmentRefs = %+v, want one ref", out.Status.VolumeAttachmentRefs)
	}
	if len(agent.receivedCmds) != 1 || agent.receivedCmds[0].Op != HotplugOpAddDisk {
		t.Fatalf("compute-agent received %+v, want exactly one ADD_DISK command", agent.receivedCmds)
	}

	// Idempotent re-attach: no second hotplug command.
	if _, err := r.LiveAttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", ""); err != nil {
		t.Fatalf("idempotent re-attach: %v", err)
	}
	if len(agent.receivedCmds) != 1 {
		t.Fatalf("idempotent re-attach sent another hotplug command: %+v", agent.receivedCmds)
	}
}

func TestReconciler_LiveAttachVolumeCleansUpOnHotplugFailure(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := runningVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})
	startFakeHotplugAgent(t, ctx, r.nc, r.js, "hypervisor-1", HotplugResult{Success: false, Error: "cloud-hypervisor: add-disk failed"})

	if _, err := r.LiveAttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", ""); err == nil {
		t.Fatal("expected LiveAttachVolume to fail when compute-agent reports failure")
	}

	// The orphaned VolumeAttachment must have been cleaned up: a retry
	// creates a fresh one rather than finding a stale lock.
	got, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.Volumes) != 0 {
		t.Fatalf("Spec.Volumes = %+v, want none (attach failed)", got.Spec.Volumes)
	}
}

func TestReconciler_LiveDetachVolumeSuccess(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := runningVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})
	agent := startFakeHotplugAgent(t, ctx, r.nc, r.js, "hypervisor-1", HotplugResult{Success: true})

	attached, err := r.LiveAttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", "")
	if err != nil {
		t.Fatalf("LiveAttachVolume: %v", err)
	}

	detached, err := r.LiveDetachVolume(ctx, tenant, attached.Meta.ID, "vol-1")
	if err != nil {
		t.Fatalf("LiveDetachVolume: %v", err)
	}
	if len(detached.Spec.Volumes) != 0 {
		t.Fatalf("Spec.Volumes after detach = %+v, want none", detached.Spec.Volumes)
	}
	if len(detached.Status.VolumeAttachmentRefs) != 0 {
		t.Fatalf("Status.VolumeAttachmentRefs after detach = %+v, want none", detached.Status.VolumeAttachmentRefs)
	}

	var ops []HotplugOp
	for _, c := range agent.receivedCmds {
		ops = append(ops, c.Op)
	}
	if len(ops) != 2 || ops[0] != HotplugOpAddDisk || ops[1] != HotplugOpRemoveDevice {
		t.Fatalf("compute-agent received ops %v, want [ADD_DISK REMOVE_DEVICE]", ops)
	}
}

func TestReconciler_LiveDetachVolumeIdempotentIfNotAttached(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)
	const tenant = "tenant-a"

	vm := runningVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 8, 8192, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})
	startFakeHotplugAgent(t, ctx, r.nc, r.js, "hypervisor-1", HotplugResult{Success: true})

	out, err := r.LiveDetachVolume(ctx, tenant, vm.Meta.ID, "never-attached")
	if err != nil {
		t.Fatalf("LiveDetachVolume on a volume that was never attached: %v", err)
	}
	if out.Meta.ID != vm.Meta.ID {
		t.Fatalf("got a different VM back: %+v", out.Meta)
	}
}
