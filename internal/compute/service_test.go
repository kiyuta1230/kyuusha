package compute

import (
	"context"
	"testing"
	"time"
)

// playground: drives the Service the way a real client (KaaS controller,
// CLI, ...) would: Create, observe it over Watch, Update with optimistic
// concurrency, Delete, and confirm Watch reports all of it. Kept as a normal
// test so it runs (and stays honest) under `go test ./...`.
func TestPlayground_VirtualMachineLifecycleOverWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	svc := NewService()
	const tenant = "tenant-a"

	events, err := svc.Watch(ctx, tenant, 0)
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
	again, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{RecoveryPolicy: RecoveryPolicyNone})
	if err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	if again.Meta.ID != m.Meta.ID {
		t.Fatalf("idempotent Create minted a new ID: %s vs %s", again.Meta.ID, m.Meta.ID)
	}

	// Simulate the scheduler advancing the phase, with optimistic concurrency.
	m.Status.Phase = PhaseScheduled
	m.Status.Node = "node-1"
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
	resumed, err := svc.Watch(ctx, tenant, lastRV)
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
	svc := NewService()

	if _, err := svc.Create(ctx, "tenant-a", "x", VirtualMachineSpec{}); err == nil {
		t.Fatal("expected validation error for unset RecoveryPolicy")
	}
}

func TestService_GetIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	m, err := svc.Create(ctx, "tenant-a", "", VirtualMachineSpec{RecoveryPolicy: RecoveryPolicyNone})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(ctx, "tenant-b", m.Meta.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant Get: got %v, want ErrNotFound", err)
	}
}
