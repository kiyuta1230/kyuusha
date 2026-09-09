package blockstorage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestService(t *testing.T, ctx context.Context) *Service {
	t.Helper()
	svc, err := NewService(ctx, &FakeTenantClient{}, &FakeStorageAgentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// playground: drives the Service the way a real client would: Create,
// observe it over Watch, Delete, and confirm Watch reports all of it. Kept
// as a normal test so it runs (and stays honest) under `go test ./...`.
func TestPlayground_VolumeLifecycleOverWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	events, err := svc.WatchVolumes(ctx, tenant, 0)
	if err != nil {
		t.Fatalf("WatchVolumes: %v", err)
	}

	vol, err := svc.CreateVolume(ctx, tenant, "data-1", VolumeSpec{SizeGB: 10})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if vol.Status.Phase != VolumePhaseReady {
		t.Fatalf("new Volume phase = %q, want Ready (no real backend to wait on yet)", vol.Status.Phase)
	}

	// Idempotent re-Create with the same name must not mint a new ID.
	again, err := svc.CreateVolume(ctx, tenant, "data-1", VolumeSpec{SizeGB: 10})
	if err != nil {
		t.Fatalf("idempotent CreateVolume: %v", err)
	}
	if again.Meta.ID != vol.Meta.ID {
		t.Fatalf("idempotent CreateVolume minted a new ID: %s vs %s", again.Meta.ID, vol.Meta.ID)
	}

	if err := svc.DeleteVolume(ctx, tenant, vol.Meta.ID); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	wantTypes := []EventType{EventAdded, EventDeleted}
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

func TestService_CreateVolumeRejectsEmptyTenant(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.CreateVolume(ctx, "", "x", VolumeSpec{SizeGB: 10}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty tenant_id: got %v, want ErrValidation", err)
	}
}

func TestService_CreateVolumeRejectsNonPositiveSize(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.CreateVolume(ctx, "tenant-a", "x", VolumeSpec{SizeGB: 0}); !errors.Is(err, ErrValidation) {
		t.Fatalf("size_gb=0: got %v, want ErrValidation", err)
	}
	if _, err := svc.CreateVolume(ctx, "tenant-a", "y", VolumeSpec{SizeGB: -5}); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative size_gb: got %v, want ErrValidation", err)
	}
}

func TestService_GetVolumeIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	vol, err := svc.CreateVolume(ctx, "tenant-a", "", VolumeSpec{SizeGB: 10})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, err := svc.GetVolume(ctx, "tenant-b", vol.Meta.ID); err != ErrVolumeNotFound {
		t.Fatalf("cross-tenant GetVolume: got %v, want ErrVolumeNotFound", err)
	}
}
