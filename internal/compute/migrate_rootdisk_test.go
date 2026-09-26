package compute

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// fakeMigrateArtifactAgent stands in for compute-agent's
// handleMigrateArtifact (internal/compute-agent/migrateartifact.go):
// subscribes to hypervisor's CmdSubjectMigrateArtifact, replies to every
// PUSH with result, and records every command it receives (PUSH and
// DELETE both) -- same shape as fakeHotplugAgent (liveops_test.go).
type fakeMigrateArtifactAgent struct {
	nc           *nats.Conn
	hypervisor   string
	pushResult   MigrateArtifactResult
	receivedCmds []MigrateArtifactCommand
}

func startFakeMigrateArtifactAgent(t *testing.T, ctx context.Context, nc *nats.Conn, js jetstream.JetStream, hypervisor string, pushResult MigrateArtifactResult) *fakeMigrateArtifactAgent {
	t.Helper()
	a := &fakeMigrateArtifactAgent{nc: nc, hypervisor: hypervisor, pushResult: pushResult}

	stream, err := js.Stream(ctx, "COMPUTE_CMD")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "test-fake-migrate-artifact-agent-" + hypervisor,
		FilterSubject: CmdSubjectMigrateArtifact(hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var cmd MigrateArtifactCommand
		if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
			return
		}
		a.receivedCmds = append(a.receivedCmds, cmd)
		if cmd.Op == MigrateArtifactOpPush {
			payload, _ := json.Marshal(a.pushResult)
			_ = a.nc.Publish(cmd.ReplySubject, payload)
		}
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	t.Cleanup(consumeCtx.Stop)
	return a
}

func TestReconciler_MigrateTransfersRootDiskOnSuccess(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}
	agent := startFakeMigrateArtifactAgent(t, ctx, r.nc, r.js, "hypervisor-1", MigrateArtifactResult{Success: true, URL: "oci://registry:5000/kyuusha-migration/" + vm.Meta.ID + ":migrate-1", Digest: "sha256:deadbeef"})

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "", true)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	got, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != PhaseScheduled {
		t.Fatalf("phase = %q, want Scheduled (conditions: %+v)", got.Status.Phase, got.Status.Conditions)
	}
	if got.Status.Hypervisor != "hypervisor-2" {
		t.Fatalf("Hypervisor = %q, want hypervisor-2", got.Status.Hypervisor)
	}
	if got.Status.PendingRootDiskURL != "oci://registry:5000/kyuusha-migration/"+vm.Meta.ID+":migrate-1" || got.Status.PendingRootDiskDigest != "sha256:deadbeef" {
		t.Fatalf("PendingRootDiskURL/Digest = %q/%q, want the pushed artifact's values", got.Status.PendingRootDiskURL, got.Status.PendingRootDiskDigest)
	}
	if len(agent.receivedCmds) != 1 || agent.receivedCmds[0].Op != MigrateArtifactOpPush || agent.receivedCmds[0].VMID != vm.Meta.ID {
		t.Fatalf("old hypervisor received commands = %+v, want exactly one PUSH for %s", agent.receivedCmds, vm.Meta.ID)
	}
}

// TestReconciler_MigrateFailsWhenRootDiskTransferFails confirms a failed
// push is reported as a failed migration (Unmigratable), not silently
// downgraded to the cheap re-clone-from-Image path -- and that the new
// Hypervisor's just-made reservation is rolled back, and the old
// Hypervisor's reservation is left untouched (never released, no
// DeleteCommand sent) since the VM never actually left it.
func TestReconciler_MigrateFailsWhenRootDiskTransferFails(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}
	startFakeMigrateArtifactAgent(t, ctx, r.nc, r.js, "hypervisor-1", MigrateArtifactResult{Success: false, Error: "registry unreachable"})

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "", true)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	got, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.Phase != PhaseMigrating {
		t.Fatalf("phase = %q, want still Migrating (transfer failed)", got.Status.Phase)
	}
	if got.Status.Hypervisor != "hypervisor-1" {
		t.Fatalf("Hypervisor = %q, want unchanged hypervisor-1", got.Status.Hypervisor)
	}
	if !got.Status.TransferRootDisk {
		t.Fatalf("TransferRootDisk cleared after a failed attempt, want left set for retry")
	}
	found := false
	for _, c := range got.Status.Conditions {
		if c.Type == "Unmigratable" && c.Reason == "RootDiskTransferFailed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("conditions = %+v, want an Unmigratable/RootDiskTransferFailed condition", got.Status.Conditions)
	}

	h2, err := svc.GetHypervisor(ctx, "hypervisor-2")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-2: %v", err)
	}
	if h2.Status.AllocatedVCPU != 0 || h2.Status.AllocatedMemoryMB != 0 {
		t.Fatalf("hypervisor-2 reservation not rolled back: %+v", h2.Status)
	}
	h1, err := svc.GetHypervisor(ctx, "hypervisor-1")
	if err != nil {
		t.Fatalf("GetHypervisor hypervisor-1: %v", err)
	}
	if h1.Status.AllocatedVCPU != spec.VCPU || h1.Status.AllocatedMemoryMB != spec.MemoryMB {
		t.Fatalf("hypervisor-1 reservation released despite the VM never actually leaving it: %+v", h1.Status)
	}
}

// TestReconciler_ProvisionUsesTransferredRootDiskThenCreateResultCleansItUp
// drives the rest of the flow past TestReconciler_MigrateTransfersRootDiskOnSuccess:
// PhaseScheduled's provisionAndPublish must clone from the transferred
// artifact (not the Image's own rootfs URL, which FakeImageClient leaves
// empty), and a subsequent successful CreateResult must clean up the
// temporary artifact and clear PendingRootDiskURL/Digest.
func TestReconciler_ProvisionUsesTransferredRootDiskThenCreateResultCleansItUp(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	r := newTestReconciler(t, ctx, svc)

	spec := VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}
	vm := stoppedVMWithHypervisor(t, ctx, svc, "tenant-a", "hypervisor-1", 8, 16384, spec)
	if _, err := svc.RegisterHypervisor(ctx, "hypervisor-2", "zone-a", 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
		t.Fatalf("RegisterHypervisor hypervisor-2: %v", err)
	}
	pushedURL := "oci://registry:5000/kyuusha-migration/" + vm.Meta.ID + ":migrate-1"
	startFakeMigrateArtifactAgent(t, ctx, r.nc, r.js, "hypervisor-1", MigrateArtifactResult{Success: true, URL: pushedURL, Digest: "sha256:deadbeef"})
	newAgent := startFakeMigrateArtifactAgent(t, ctx, r.nc, r.js, "hypervisor-2", MigrateArtifactResult{})

	migrating, err := svc.Migrate(ctx, "tenant-a", vm.Meta.ID, "", true)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	r.migrateVM(ctx, *migrating)

	scheduled, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get after migrateVM: %v", err)
	}

	var receivedCreates []CreateCommand
	stream, err := r.js.Stream(ctx, "COMPUTE_CMD")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "test-create-watcher",
		FilterSubject: CmdSubjectCreate("hypervisor-2"),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var cmd CreateCommand
		if err := json.Unmarshal(msg.Data(), &cmd); err == nil {
			receivedCreates = append(receivedCreates, cmd)
		}
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	defer consumeCtx.Stop()

	r.reconcile(ctx, *scheduled)

	waitFor(t, func() bool { return len(receivedCreates) == 1 })
	if receivedCreates[0].RootfsURL != pushedURL || receivedCreates[0].RootfsDigest != "sha256:deadbeef" {
		t.Fatalf("CreateCommand.RootfsURL/Digest = %q/%q, want the transferred artifact's values", receivedCreates[0].RootfsURL, receivedCreates[0].RootfsDigest)
	}

	provisioning, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get after provisionAndPublish: %v", err)
	}
	if provisioning.Status.PendingRootDiskURL != pushedURL {
		t.Fatalf("PendingRootDiskURL cleared before cleanup ran, want still %q", pushedURL)
	}

	r.handleCreateResult(ctx, CreateResult{VMID: vm.Meta.ID, Success: true})

	waitFor(t, func() bool {
		for _, c := range newAgent.receivedCmds {
			if c.Op == MigrateArtifactOpDelete && c.URL == pushedURL {
				return true
			}
		}
		return false
	})

	final, err := svc.Get(ctx, "tenant-a", vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get after handleCreateResult: %v", err)
	}
	if final.Status.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want Running", final.Status.Phase)
	}
	if final.Status.PendingRootDiskURL != "" || final.Status.PendingRootDiskDigest != "" {
		t.Fatalf("PendingRootDiskURL/Digest not cleared after cleanup: %q/%q", final.Status.PendingRootDiskURL, final.Status.PendingRootDiskDigest)
	}
}
