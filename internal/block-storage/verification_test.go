package blockstorage

import (
	"context"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestStorageConnection_ReadyRequiresAllDeclaredZones exercises the strict
// policy from docs/open-questions.md「Hypervisor↔ストレージバックエンドの
// 接続確立をkyuusha側で自動化すべきか」: a StorageConnection declaring
// multiple zones stays Pending until every one of them is confirmed, not
// just any one.
func TestStorageConnection_ReadyRequiresAllDeclaredZones(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sc, err := svc.CreateStorageConnection(ctx, "multi-zone", StorageConnectionSpec{Zones: []string{"zone-a", "zone-b"}})
	if err != nil {
		t.Fatalf("CreateStorageConnection: %v", err)
	}
	if sc.Status.Phase != StorageConnectionPhasePending {
		t.Fatalf("phase = %q, want Pending before any zone is confirmed", sc.Status.Phase)
	}

	// Only zone-a confirmed: still not enough.
	svc.recordHypervisorConnections(ctx, "hypervisor-1", "zone-a", []string{"multi-zone"})
	got, err := svc.GetStorageConnection(ctx, sc.Meta.ID)
	if err != nil {
		t.Fatalf("GetStorageConnection: %v", err)
	}
	if got.Status.Phase != StorageConnectionPhasePending {
		t.Fatalf("phase = %q, want still Pending with only zone-a confirmed", got.Status.Phase)
	}

	// zone-b confirmed too: now every declared zone is covered.
	svc.recordHypervisorConnections(ctx, "hypervisor-2", "zone-b", []string{"multi-zone"})
	got, err = svc.GetStorageConnection(ctx, sc.Meta.ID)
	if err != nil {
		t.Fatalf("GetStorageConnection: %v", err)
	}
	if got.Status.Phase != StorageConnectionPhaseReady {
		t.Fatalf("phase = %q, want Ready once every declared zone is confirmed", got.Status.Phase)
	}
	if len(got.Status.VerifiedZones) != 2 {
		t.Fatalf("verified_zones = %v, want both zones", got.Status.VerifiedZones)
	}
}

// TestVolume_ReadyRequiresBothConnectionAndIdentifierVerification exercises
// the two-layer design: a Volume needs its StorageConnection Ready *and*
// its own identifier separately confirmed, and reaching one without the
// other must not promote it.
func TestVolume_ReadyRequiresBothConnectionAndIdentifierVerification(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceNoConnection(t, ctx)

	if _, err := svc.CreateStorageConnection(ctx, "conn-x", StorageConnectionSpec{Zones: []string{"zone-a"}}); err != nil {
		t.Fatalf("CreateStorageConnection: %v", err)
	}
	// StorageConnection stays Pending -- no Hypervisor has reported it yet.

	vol, err := svc.CreateVolume(ctx, "tenant-a", "vol-1", VolumeSpec{
		SizeGB: 10, Protocol: StorageProtocolNFS, StorageConnection: "conn-x", Identifier: "vol-1.img",
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if vol.Status.Phase != VolumePhasePending {
		t.Fatalf("phase = %q, want Pending", vol.Status.Phase)
	}

	// Identifier verified, but the StorageConnection still isn't Ready:
	// must not promote yet.
	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: vol.Meta.ID, Success: true, SizeBytes: 10 << 30})
	got, err := svc.GetVolume(ctx, "tenant-a", vol.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if got.Status.Phase != VolumePhasePending {
		t.Fatalf("phase = %q, want still Pending (StorageConnection not Ready yet)", got.Status.Phase)
	}

	// Now the connection becomes Ready (its one declared zone gets
	// confirmed): sweepPendingVolumes should pick up the already-verified
	// identifier and promote the Volume the rest of the way.
	svc.recordHypervisorConnections(ctx, "hypervisor-1", "zone-a", []string{"conn-x"})
	svc.sweepPendingVolumes(ctx)
	got, err = svc.GetVolume(ctx, "tenant-a", vol.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if got.Status.Phase != VolumePhaseReady {
		t.Fatalf("phase = %q, want Ready once both the connection and the identifier are verified", got.Status.Phase)
	}
}

// TestVolume_FailedVerificationStaysPendingNotError confirms the
// deliberate "never Error, just keep retrying" policy: a failed
// verify-result must not sour the Volume permanently.
func TestVolume_FailedVerificationStaysPendingNotError(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx) // "test-connection" already forced Ready

	vol, err := svc.CreateVolume(ctx, "tenant-a", "vol-1", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: vol.Meta.ID, Success: false, Error: "no such file"})
	got, err := svc.GetVolume(ctx, "tenant-a", vol.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if got.Status.Phase != VolumePhasePending {
		t.Fatalf("phase = %q, want Pending (a failed verification must never produce Error)", got.Status.Phase)
	}

	// A later, successful verification must still be able to promote it --
	// the earlier failure must not have latched permanently.
	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: vol.Meta.ID, Success: true, SizeBytes: 10 << 30})
	got, err = svc.GetVolume(ctx, "tenant-a", vol.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if got.Status.Phase != VolumePhaseReady {
		t.Fatalf("phase = %q, want Ready after a subsequent successful verification", got.Status.Phase)
	}
}

// newTestServiceNoConnection is newTestService without pre-creating
// "test-connection" -- for tests that want to control StorageConnection
// creation/verification timing themselves.
func newTestServiceNoConnection(t *testing.T, ctx context.Context) *Service {
	t.Helper()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}
