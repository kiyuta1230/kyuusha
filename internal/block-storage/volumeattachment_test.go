package blockstorage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestService_CreateVolumeAttachmentValidatesVolume(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "x", VolumeAttachmentSpec{
		VMID: "vm-1", VolumeID: "volume-does-not-exist",
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown volume_id: got %v, want ErrValidation", err)
	}
}

// TestService_HandleVolumeAttachedRecordsDevicePathAndHypervisor exercises
// the report-back path from docs/specs/volume.md
// "status.device_path/status.hypervisor": compute-agent's proactive
// VolumeAttachedEvent (fired at a real successful boot, see agent.go's
// reportAttachedVolumes) must land on the right VolumeAttachment without
// disturbing its Phase (that's exclusive-attach bookkeeping's job, not
// this report's).
func TestService_HandleVolumeAttachedRecordsDevicePathAndHypervisor(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	vol, err := svc.CreateVolume(ctx, "tenant-a", "data-1", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vol = forceVolumeVerified(t, ctx, svc, vol)
	att, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "volattach-vm-1", VolumeAttachmentSpec{
		VMID: "vm-1", VolumeID: vol.Meta.ID,
	})
	if err != nil {
		t.Fatalf("CreateVolumeAttachment: %v", err)
	}

	svc.handleVolumeAttached(ctx, VolumeAttachedEvent{
		AttachmentID: att.Meta.ID, TenantID: "tenant-a", Hypervisor: "hypervisor-1", DevicePath: "/dev/disk/by-id/scsi-test-serial",
	})

	got, err := svc.GetVolumeAttachment(ctx, "tenant-a", att.Meta.ID)
	if err != nil {
		t.Fatalf("GetVolumeAttachment: %v", err)
	}
	if got.Status.Hypervisor != "hypervisor-1" {
		t.Fatalf("Hypervisor = %q, want hypervisor-1", got.Status.Hypervisor)
	}
	if got.Status.DevicePath != "/dev/disk/by-id/scsi-test-serial" {
		t.Fatalf("DevicePath = %q, want /dev/disk/by-id/scsi-test-serial", got.Status.DevicePath)
	}
	if got.Status.Phase != VolumeAttachmentPhaseAttached {
		t.Fatalf("Phase = %q, want unchanged Attached (this report must not touch it)", got.Status.Phase)
	}
}

// TestService_HandleVolumeAttachedIgnoresUnknownAttachment confirms a
// report for an already-deleted (or never-existent) VolumeAttachment is a
// silent no-op, not a panic or a spurious Create.
func TestService_HandleVolumeAttachedIgnoresUnknownAttachment(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	svc.handleVolumeAttached(ctx, VolumeAttachedEvent{
		AttachmentID: "volattach-does-not-exist", TenantID: "tenant-a", Hypervisor: "hypervisor-1", DevicePath: "/dev/sdz",
	})
}

func TestService_VolumeAttachmentAttachesWhenVolumeIsFree(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	vol, err := svc.CreateVolume(ctx, "tenant-a", "data-1", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vol = forceVolumeVerified(t, ctx, svc, vol)
	att, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "volattach-vm-1", VolumeAttachmentSpec{
		VMID: "vm-1", VolumeID: vol.Meta.ID,
	})
	if err != nil {
		t.Fatalf("CreateVolumeAttachment: %v", err)
	}
	if att.Status.Phase != VolumeAttachmentPhaseAttached {
		t.Fatalf("phase = %q, want Attached", att.Status.Phase)
	}
}

// TestService_ExclusiveAttachBlocksSecondAttachmentThenRetrySucceeds
// exercises docs/architecture.md's "具体的な排他制御": a second
// VolumeAttachment for the same volume_id must not attach while the first
// is still active, but must succeed once the first is deleted -- either
// immediately (a fresh CreateVolumeAttachment re-checks) or via Run's
// periodic retry sweep for one already sitting Pending.
func TestService_ExclusiveAttachBlocksSecondAttachmentThenRetrySucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	svc := newTestService(t, ctx)

	vol, err := svc.CreateVolume(ctx, "tenant-a", "data-1", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vol = forceVolumeVerified(t, ctx, svc, vol)
	first, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "volattach-vm-1", VolumeAttachmentSpec{
		VMID: "vm-1", VolumeID: vol.Meta.ID,
	})
	if err != nil {
		t.Fatalf("first CreateVolumeAttachment: %v", err)
	}
	if first.Status.Phase != VolumeAttachmentPhaseAttached {
		t.Fatalf("first attachment phase = %q, want Attached", first.Status.Phase)
	}

	second, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "volattach-vm-2", VolumeAttachmentSpec{
		VMID: "vm-2", VolumeID: vol.Meta.ID,
	})
	if err != nil {
		t.Fatalf("second CreateVolumeAttachment: %v", err)
	}
	if second.Status.Phase != VolumeAttachmentPhasePending {
		t.Fatalf("second attachment (same volume, first still active) phase = %q, want Pending", second.Status.Phase)
	}

	if err := svc.DeleteVolumeAttachment(ctx, "tenant-a", first.Meta.ID); err != nil {
		t.Fatalf("DeleteVolumeAttachment (first): %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		svc.retryPendingAttachments(ctx)
		got, err := svc.GetVolumeAttachment(ctx, "tenant-a", second.Meta.ID)
		if err != nil {
			t.Fatalf("GetVolumeAttachment: %v", err)
		}
		if got.Status.Phase == VolumeAttachmentPhaseAttached {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the second attachment to attach after the first was deleted, last phase = %s", got.Status.Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestService_SweepOrphanedVolumeAttachmentsDeletesOnlyMissingVMs proves
// docs/architecture.md's orphan-GC detection logic (parent Get -> NotFound
// means delete self) against a real embedded etcd, using a
// FakeVirtualMachineClient that knows about vm-exists but not vm-gone --
// see docs/specs/volume.md "VolumeAttachmentのオーファンGC" for the leak
// this backstops (a VolumeAttachment created directly against this service
// after its VM already booted is never tracked in
// VirtualMachineStatus.VolumeAttachmentRefs, so VM Delete's active cleanup
// never reaches it).
func TestService_SweepOrphanedVolumeAttachmentsDeletesOnlyMissingVMs(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, &FakeVirtualMachineClient{Existing: map[string]bool{"vm-exists": true}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mustCreateTestStorageConnection(t, ctx, svc, "test-connection", "test-zone")

	liveVol, err := svc.CreateVolume(ctx, "tenant-a", "data-live", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume(liveVol): %v", err)
	}
	liveVol = forceVolumeVerified(t, ctx, svc, liveVol)
	orphanVol, err := svc.CreateVolume(ctx, "tenant-a", "data-orphan", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume(orphanVol): %v", err)
	}
	orphanVol = forceVolumeVerified(t, ctx, svc, orphanVol)

	live, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "volattach-live", VolumeAttachmentSpec{
		VMID: "vm-exists", VolumeID: liveVol.Meta.ID,
	})
	if err != nil {
		t.Fatalf("CreateVolumeAttachment(live): %v", err)
	}
	orphan, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "volattach-orphan", VolumeAttachmentSpec{
		VMID: "vm-gone", VolumeID: orphanVol.Meta.ID,
	})
	if err != nil {
		t.Fatalf("CreateVolumeAttachment(orphan): %v", err)
	}

	svc.sweepOrphanedVolumeAttachments(ctx)

	if _, err := svc.GetVolumeAttachment(ctx, "tenant-a", live.Meta.ID); err != nil {
		t.Fatalf("live VolumeAttachment (vm-exists) was deleted: %v", err)
	}
	if _, err := svc.GetVolumeAttachment(ctx, "tenant-a", orphan.Meta.ID); !errors.Is(err, ErrVolumeAttachmentNotFound) {
		t.Fatalf("orphaned VolumeAttachment (vm-gone) still exists: err=%v, want ErrVolumeAttachmentNotFound", err)
	}
}
