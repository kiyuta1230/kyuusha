package blockstorage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestService(t *testing.T, ctx context.Context) *Service {
	t.Helper()
	svc, err := NewService(ctx, &FakeTenantClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mustCreateTestStorageConnection(t, ctx, svc, "test-connection", "test-zone")
	return svc
}

// mustCreateTestStorageConnection creates a StorageConnection and forces it
// straight to Ready -- tests run with no real NATS/compute-agent (s.js is
// nil, see Service's own doc comment), so the normal Hypervisor-self-report
// path that would verify it for real never fires on its own.
func mustCreateTestStorageConnection(t *testing.T, ctx context.Context, svc *Service, name, zone string) {
	t.Helper()
	sc, err := svc.CreateStorageConnection(ctx, name, StorageConnectionSpec{Zones: []string{zone}})
	if err != nil {
		t.Fatalf("CreateStorageConnection: %v", err)
	}
	sc.Status.Phase = StorageConnectionPhaseReady
	sc.Status.VerifiedZones = []string{zone}
	if _, err := svc.storageConnections.Update(ctx, *sc); err != nil {
		t.Fatalf("force StorageConnection ready: %v", err)
	}
}

// forceVolumeVerified simulates compute-agent's real verify-volume-result
// arriving and succeeding, so a test that needs a Ready Volume (to then
// exercise VolumeAttachment logic, say) doesn't need a real NATS/
// compute-agent round-trip -- exercises the real promotion logic
// (handleVerifyResult), just with a synthetic result instead of one that
// actually arrived over NATS.
func forceVolumeVerified(t *testing.T, ctx context.Context, svc *Service, vol *Volume) *Volume {
	t.Helper()
	svc.handleVerifyResult(ctx, VerifyVolumeResult{
		TenantID:  vol.Meta.TenantID,
		VolumeID:  vol.Meta.ID,
		Success:   true,
		SizeBytes: vol.Spec.SizeGB * (1 << 30),
	})
	got, err := svc.GetVolume(ctx, vol.Meta.TenantID, vol.Meta.ID)
	if err != nil {
		t.Fatalf("forceVolumeVerified: GetVolume: %v", err)
	}
	return got
}

// testVolumeSpec fills in the fields every valid VolumeSpec needs beyond
// size_gb (protocol/storage_connection/identifier) with placeholder values
// -- kyuusha never actually dials anything backed by these in tests, since
// CreateVolume no longer makes an external call at all.
func testVolumeSpec(sizeGB int64) VolumeSpec {
	return VolumeSpec{
		SizeGB:            sizeGB,
		Protocol:          StorageProtocolISCSI,
		StorageConnection: "test-connection",
		Identifier:        "test-serial",
	}
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

	vol, err := svc.CreateVolume(ctx, tenant, "data-1", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if vol.Status.Phase != VolumePhasePending {
		t.Fatalf("new Volume phase = %q, want Pending (verification is async -- see verification.go)", vol.Status.Phase)
	}

	// Idempotent re-Create with the same name must not mint a new ID.
	again, err := svc.CreateVolume(ctx, tenant, "data-1", testVolumeSpec(10))
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

	if _, err := svc.CreateVolume(ctx, "", "x", testVolumeSpec(10)); !errors.Is(err, ErrValidation) {
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

	vol, err := svc.CreateVolume(ctx, "tenant-a", "", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, err := svc.GetVolume(ctx, "tenant-b", vol.Meta.ID); err != ErrVolumeNotFound {
		t.Fatalf("cross-tenant GetVolume: got %v, want ErrVolumeNotFound", err)
	}
}
