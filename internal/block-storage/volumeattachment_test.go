package blockstorage

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestService_VolumeAttachmentAttachesWhenVolumeIsFree(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	vol, err := svc.CreateVolume(ctx, "tenant-a", "data-1", VolumeSpec{SizeGB: 10})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc := newTestService(t, ctx)

	vol, err := svc.CreateVolume(ctx, "tenant-a", "data-1", VolumeSpec{SizeGB: 10})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
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
