package identity

import (
	"context"
	"testing"
	"time"
)

func TestPlayground_TenantLifecycleOverWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	svc := NewService()

	events, err := svc.Watch(ctx, "", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	tn, err := svc.Create(ctx, "acme", TenantSpec{
		DisplayName: "Acme Corp",
		Quota:       QuotaSpec{MaxVCPU: 64, MaxMemoryMB: 262144, MaxVMs: 50},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tn.Status.Phase != PhaseActive {
		t.Fatalf("new Tenant phase = %q, want %q", tn.Status.Phase, PhaseActive)
	}
	if tn.Meta.TenantID != tn.Meta.ID {
		t.Fatalf("Tenant not self-referential: id=%s tenant_id=%s", tn.Meta.ID, tn.Meta.TenantID)
	}

	// Idempotent re-Create with the same name must not mint a new ID.
	again, err := svc.Create(ctx, "acme", TenantSpec{})
	if err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	if again.Meta.ID != tn.Meta.ID {
		t.Fatalf("idempotent Create minted a new ID: %s vs %s", again.Meta.ID, tn.Meta.ID)
	}

	// Self-Get: a Tenant is looked up by its own id used as the tenantID.
	got, err := svc.Get(ctx, tn.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.DisplayName != "Acme Corp" {
		t.Fatalf("Get returned wrong tenant: %+v", got)
	}

	got.Spec.Quota.MaxVCPU = 128
	updated, err := svc.Update(ctx, got)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := svc.Update(ctx, got); err != ErrConflict {
		t.Fatalf("stale Update: got %v, want ErrConflict", err)
	}

	if err := svc.Delete(ctx, updated.Meta.ID); err != nil {
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
}

func TestService_GetIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	a, err := svc.Create(ctx, "tenant-a", TenantSpec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(ctx, a.Meta.ID+"-nope"); err != ErrNotFound {
		t.Fatalf("Get with wrong id: got %v, want ErrNotFound", err)
	}
}

func TestService_CreateNamesAreGloballyUnique(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	a, err := svc.Create(ctx, "acme", TenantSpec{DisplayName: "first"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := svc.Create(ctx, "acme", TenantSpec{DisplayName: "second"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.Meta.ID != b.Meta.ID || b.Spec.DisplayName != "first" {
		t.Fatalf("second Create with same name did not return the first Tenant: a=%+v b=%+v", a, b)
	}
}
