// Package playground wires real, if minimal, kyuusha components together
// (an embedded NATS/JetStream server, the compute service, its Reconciler,
// and a compute-agent) and drives them the way a live deployment would. Kept
// as `go test` so it stays honest as the system grows; run it directly with
// `go test ./playground/... -run TestPlayground -v`.
package playground

import (
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"gitlab.com/ki.yuta1230/kyuusha/internal/compute"
	"gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent"
)

func startNATS(t *testing.T) *nats.Conn {
	t.Helper()

	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random free port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
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
	return nc
}

// TestPlayground_CreateVMReachesRunning drives the full path a real
// deployment would use: compute.Service (the "user interface") -> Reconciler
// (schedules, publishes ms.compute.cmd.*.vm.create) -> compute-agent
// (stub-creates, publishes ms.compute.evt.*.vm.create-result) -> Reconciler
// marks the VM Running. No Firecracker/QEMU involved yet.
func TestPlayground_CreateVMReachesRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	nc := startNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}

	svc := compute.NewService()
	recon := compute.NewReconciler(svc, nc, js, []string{"hypervisor-1"})
	go func() {
		if err := recon.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("reconciler.Run: %v", err)
		}
	}()

	agent := &computeagent.Agent{Hypervisor: "hypervisor-1", NC: nc, JS: js, HeartbeatInterval: 200 * time.Millisecond}
	go func() {
		if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("agent.Run: %v", err)
		}
	}()

	vm, err := svc.Create(ctx, "tenant-a", "web-1", compute.VirtualMachineSpec{
		ImageID:        "img-abc",
		VCPU:           2,
		MemoryMB:       4096,
		RecoveryPolicy: compute.RecoveryPolicyNone,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		got, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status.Phase == compute.PhaseRunning {
			if got.Status.Hypervisor != "hypervisor-1" {
				t.Fatalf("Hypervisor = %q, want hypervisor-1", got.Status.Hypervisor)
			}
			return
		}
		if got.Status.Phase == compute.PhaseError {
			t.Fatalf("VM went to Error: %+v", got.Status.Conditions)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Running, last phase = %s", got.Status.Phase)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
