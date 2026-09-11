package compute

import (
	"context"
	"errors"
	"testing"
	"time"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"gitlab.com/ki.yuta1230/kyuusha/internal/authn"
	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
	"gitlab.com/ki.yuta1230/kyuusha/internal/resourcetest"
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
		ImageID:        "img-abc",
		VCPU:           2,
		MemoryMB:       4096,
		RecoveryPolicy: RecoveryPolicyNone,
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
	again, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{ImageID: "img-abc", RecoveryPolicy: RecoveryPolicyNone})
	if err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	if again.Meta.ID != m.Meta.ID {
		t.Fatalf("idempotent Create minted a new ID: %s vs %s", again.Meta.ID, m.Meta.ID)
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

	if err := svc.Delete(ctx, tenant, updated.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	wantTypes := []EventType{EventAdded, EventModified, EventDeleted}
	for _, want := range wantTypes {
		select {
		case e := <-events:
			if e.Type != want {
				t.Fatalf("event type = %s, want %s", e.Type, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s event", want)
		}
	}

	// A fresh Watch resumed from the last known resource_version should see
	// nothing new (no relist error, empty backlog) since we're caught up.
	lastRV := updated.Meta.ResourceVersion + 1 // +1 for the Delete event
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

func TestService_CreateRejectsUnspecifiedRecoveryPolicy(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.Create(ctx, "tenant-a", "x", VirtualMachineSpec{}); err == nil {
		t.Fatal("expected validation error for unset RecoveryPolicy")
	}
}

func TestService_GetIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	m, err := svc.Create(ctx, "tenant-a", "", VirtualMachineSpec{ImageID: "img-abc", RecoveryPolicy: RecoveryPolicyNone})
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
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512, RecoveryPolicy: RecoveryPolicyNone,
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
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512, RecoveryPolicy: RecoveryPolicyNone,
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
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512, RecoveryPolicy: RecoveryPolicyNone,
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
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512, RecoveryPolicy: RecoveryPolicyNone,
	})
	if err != nil {
		t.Fatalf("Create vm-a: %v", err)
	}
	vmB, err := svc.Create(ctx, tenant, "vm-b", VirtualMachineSpec{
		ImageID: "img-abc", VCPU: 1, MemoryMB: 512, RecoveryPolicy: RecoveryPolicyNone,
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
