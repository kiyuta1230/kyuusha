package blockstorage

import (
	"context"
	"testing"
	"time"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestStorageConnection_ReadyRequiresAllDeclaredZones exercises the strict
// policy from docs/open-questions.md「Hypervisor↔ストレージバックエンドの
// 接続確立をkyuusha側で自動化すべきか」: a StorageConnection declaring
// multiple zones stays Pending until every one of them is confirmed, not
// just any one.
func TestStorageConnection_ReadyRequiresAllDeclaredZones(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil)
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

// TestVolume_SizeMismatchIsAutoCorrected exercises correctDeclaredSize
// (docs/open-questions.md「Volumeの申告内容...」): the same round-trip that
// confirms a Volume's identifier exists also reports its real observed
// size; a declared size_gb that drifted beyond tolerance is corrected in
// place (not just flagged), and tenant_usage.VolumeGB is adjusted to match.
func TestVolume_SizeMismatchIsAutoCorrected(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx) // "test-connection" already forced Ready

	matching, err := svc.CreateVolume(ctx, "tenant-a", "vol-matching", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume (matching): %v", err)
	}
	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: matching.Meta.ID, Success: true, SizeBytes: 10 << 30})
	got, err := svc.GetVolume(ctx, "tenant-a", matching.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolume (matching): %v", err)
	}
	if !hasCondition(got.Status.Conditions, conditionSizeMatchesDeclaration, resource.ConditionTrue) {
		t.Fatalf("matching size: conditions = %+v, want SizeMatchesDeclaration=True", got.Status.Conditions)
	}
	if got.Spec.SizeGB != 10 {
		t.Fatalf("matching size: spec.size_gb = %d, want unchanged 10 (already matched, nothing to correct)", got.Spec.SizeGB)
	}

	mismatched, err := svc.CreateVolume(ctx, "tenant-a", "vol-mismatched", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume (mismatched): %v", err)
	}
	svc.usageMu.Lock()
	usageBefore := svc.usage["tenant-a"].VolumeGB
	svc.usageMu.Unlock()

	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: mismatched.Meta.ID, Success: true, SizeBytes: 1 << 30})
	got, err = svc.GetVolume(ctx, "tenant-a", mismatched.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolume (mismatched): %v", err)
	}
	// declared 10 GB, really 1 GiB (1<<30 bytes) -- corrected to 1 GB.
	if got.Spec.SizeGB != 1 {
		t.Fatalf("mismatched size: spec.size_gb = %d, want corrected to 1 (the real observed size)", got.Spec.SizeGB)
	}
	if !hasCondition(got.Status.Conditions, conditionSizeMatchesDeclaration, resource.ConditionTrue) {
		t.Fatalf("mismatched size: conditions = %+v, want SizeMatchesDeclaration=True (declaration now equals the corrected value)", got.Status.Conditions)
	}
	if !hasCondition(got.Status.Conditions, conditionQuotaExceededAfterCorrection, resource.ConditionFalse) {
		t.Fatalf("mismatched size: conditions = %+v, want QuotaExceededAfterCorrection=False (well within the unlimited test quota)", got.Status.Conditions)
	}
	// A size correction is not a failure: existence was still confirmed, so
	// the Volume must still reach Ready.
	if got.Status.Phase != VolumePhaseReady {
		t.Fatalf("mismatched size: phase = %q, want still Ready (a size correction is not fatal)", got.Status.Phase)
	}

	svc.usageMu.Lock()
	usageAfter := svc.usage["tenant-a"].VolumeGB
	svc.usageMu.Unlock()
	if usageAfter != usageBefore-10+1 {
		t.Fatalf("tenant_usage.volume_gb after correction = %d, want %d (before %d, -10 declared +1 corrected)", usageAfter, usageBefore-10+1, usageBefore)
	}
}

// TestVolume_SizeCorrectionExceedingQuotaIsFlaggedNotBlocked confirms
// option A from the design discussion: a correction that pushes a tenant
// over max_volume_gb is still applied (never retroactively destroys or
// blocks an already-existing Volume) but is surfaced via
// QuotaExceededAfterCorrection for an admin to notice.
func TestVolume_SizeCorrectionExceedingQuotaIsFlaggedNotBlocked(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{MaxVolumeGb: 5}}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mustCreateTestStorageConnection(t, ctx, svc, "test-connection", "test-zone")

	// Declared small enough to fit under the 5 GB quota at Create time.
	vol, err := svc.CreateVolume(ctx, "tenant-a", "vol-1", testVolumeSpec(2))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	// Real size turns out to be far larger than declared -- correction
	// pushes tenant_usage past the 5 GB cap.
	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: vol.Meta.ID, Success: true, SizeBytes: 20 << 30})
	got, err := svc.GetVolume(ctx, "tenant-a", vol.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if got.Spec.SizeGB != 20 {
		t.Fatalf("spec.size_gb = %d, want corrected to 20 despite exceeding quota (correction is never blocked)", got.Spec.SizeGB)
	}
	if !hasCondition(got.Status.Conditions, conditionQuotaExceededAfterCorrection, resource.ConditionTrue) {
		t.Fatalf("conditions = %+v, want QuotaExceededAfterCorrection=True", got.Status.Conditions)
	}
	if got.Status.Phase != VolumePhaseReady {
		t.Fatalf("phase = %q, want still Ready (over-quota-after-correction is flagged, not fatal)", got.Status.Phase)
	}
}

// newTestServiceNoConnection is newTestService without pre-creating
// "test-connection" -- for tests that want to control StorageConnection
// creation/verification timing themselves.
func newTestServiceNoConnection(t *testing.T, ctx context.Context) *Service {
	t.Helper()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestVerifyEligibleAndAdvance_BacksOffExponentially is the regression test
// for the previously-untuned retry docs/specs/volume.md called out
// (「検証コマンドのリトライ間隔・上限は未チューニング」): a Volume that keeps
// failing verification must be asked about less and less often, not on
// every single tick forever.
func TestVerifyEligibleAndAdvance_BacksOffExponentially(t *testing.T) {
	svc := &Service{}
	const volID = "vol-1"

	if !svc.verifyEligibleAndAdvance(volID) {
		t.Fatal("first attempt: want eligible")
	}
	if svc.verifyEligibleAndAdvance(volID) {
		t.Fatal("immediate re-check: want NOT eligible (backoff just scheduled)")
	}

	svc.verifyBackoffMu.Lock()
	if got := svc.verifyBackoff[volID].interval; got != verifyRetryInterval {
		t.Fatalf("interval after 1st attempt = %v, want %v", got, verifyRetryInterval)
	}
	// Pretend the backoff has already elapsed, rather than sleeping for real.
	state := svc.verifyBackoff[volID]
	state.next = time.Now().Add(-time.Second)
	svc.verifyBackoff[volID] = state
	svc.verifyBackoffMu.Unlock()

	if !svc.verifyEligibleAndAdvance(volID) {
		t.Fatal("2nd attempt once backoff elapsed: want eligible")
	}
	svc.verifyBackoffMu.Lock()
	got := svc.verifyBackoff[volID].interval
	svc.verifyBackoffMu.Unlock()
	if want := verifyRetryInterval * 2; got != want {
		t.Fatalf("interval after 2nd attempt = %v, want %v (doubled)", got, want)
	}
}

func TestVerifyEligibleAndAdvance_CapsAtMax(t *testing.T) {
	svc := &Service{verifyBackoff: map[string]verifyRetryState{
		"vol-1": {next: time.Now().Add(-time.Second), interval: verifyRetryMax},
	}}
	if !svc.verifyEligibleAndAdvance("vol-1") {
		t.Fatal("want eligible once due")
	}
	svc.verifyBackoffMu.Lock()
	got := svc.verifyBackoff["vol-1"].interval
	svc.verifyBackoffMu.Unlock()
	if got != verifyRetryMax {
		t.Fatalf("interval = %v, want capped at %v (not doubled past the cap)", got, verifyRetryMax)
	}
}

// TestClearVerifyBackoff_ResetsState confirms a Volume that succeeds after
// previously failing starts fresh (verifyRetryInterval) if it ever needs
// re-verification again, rather than inheriting however far it had backed
// off before.
func TestClearVerifyBackoff_ResetsState(t *testing.T) {
	svc := &Service{}
	const volID = "vol-1"
	svc.verifyEligibleAndAdvance(volID) // seed backoff state
	svc.clearVerifyBackoff(volID)

	svc.verifyBackoffMu.Lock()
	_, ok := svc.verifyBackoff[volID]
	svc.verifyBackoffMu.Unlock()
	if ok {
		t.Fatal("verifyBackoff state should be cleared")
	}
	if !svc.verifyEligibleAndAdvance(volID) {
		t.Fatal("after clear, should be immediately eligible again")
	}
}

// TestHandleVerifyResult_ClearsBackoffOnSuccess confirms the real success
// path (not just the standalone helper) actually clears backoff state.
// Backoff itself is scheduled at the point a verify attempt is SENT
// (verifyEligibleAndAdvance, called from sweepPendingVolumes/
// watchPendingVolumes before publishing) rather than when a reply comes
// back, so this simulates that ordering directly rather than going through
// the NATS-dependent sweep.
func TestHandleVerifyResult_ClearsBackoffOnSuccess(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	vol, err := svc.CreateVolume(ctx, "tenant-a", "vol-1", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	if !svc.verifyEligibleAndAdvance(vol.Meta.ID) {
		t.Fatal("first attempt: want eligible")
	}
	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: vol.Meta.ID, Success: false, Error: "no such file"})
	svc.verifyBackoffMu.Lock()
	_, backedOff := svc.verifyBackoff[vol.Meta.ID]
	svc.verifyBackoffMu.Unlock()
	if !backedOff {
		t.Fatal("a failed verification's already-scheduled backoff state should still be there")
	}

	svc.handleVerifyResult(ctx, VerifyVolumeResult{TenantID: "tenant-a", VolumeID: vol.Meta.ID, Success: true, SizeBytes: 10 * 1024 * 1024 * 1024})
	svc.verifyBackoffMu.Lock()
	_, stillBackedOff := svc.verifyBackoff[vol.Meta.ID]
	svc.verifyBackoffMu.Unlock()
	if stillBackedOff {
		t.Fatal("a successful verification must clear backoff state")
	}
}
