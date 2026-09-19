package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

// validateVolumes implements the same "never create a doomed
// VirtualMachine" rule as validateImage/validateNetworkInterfaces: every
// referenced Volume must exist, belong to tenantID, and be Ready. It does
// NOT check the exclusive-attach constraint (docs/architecture.md「具体的な
// 排他制御」) -- that's block-storage's own job at actual VolumeAttachment
// Create time (reconciler.go's PhaseScheduled branch), which can only be
// as fresh as the moment it runs, same reasoning as re-deriving the
// network zone there instead of trusting this Create-time check.
func validateVolumes(ctx context.Context, client blockstoragev1.VolumeServiceClient, tenantID string, volumes []VolumeRequest) error {
	for _, v := range volumes {
		if v.VolumeID == "" {
			return fmt.Errorf("%w: volumes[].volume_id is required", ErrValidation)
		}
		vol, err := client.Get(ctx, &blockstoragev1.GetVolumeRequest{TenantId: tenantID, Id: v.VolumeID})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return fmt.Errorf("%w: volume_id %q does not exist", ErrValidation, v.VolumeID)
			}
			return err
		}
		if vol.GetStatus().GetPhase() != "Ready" {
			return fmt.Errorf("%w: volume %q is not Ready (phase=%s)", ErrValidation, v.VolumeID, vol.GetStatus().GetPhase())
		}
	}
	return nil
}

// volumeAttachmentName is createVolumeAttachments' deterministic
// child-resource name (docs/architecture.md's "子リソースIDの決定的生成
// ルール"), keyed by VolumeID rather than list position. AttachVolume/
// DetachVolume (Service, see service.go) can append/remove anywhere in
// vm.Spec.Volumes, so a position-keyed name would shift out from under an
// existing attachment the moment anything before it in the slice is
// removed -- see legacyVolumeAttachmentName below for the pre-existing
// scheme this replaces and how the two coexist.
func volumeAttachmentName(vmID, volumeID string) string {
	return fmt.Sprintf("volattach-%s-%s", vmID, volumeID)
}

// legacyVolumeAttachmentName is the original volattach-<vm_id>-<index>
// scheme (position-keyed), still tried first in createVolumeAttachments so
// that a VM whose vm.Spec.Volumes has never been reordered by DetachVolume
// keeps resolving to its original attachment forever -- no migration job,
// no rename primitive needed (resource.Store has none). Only once
// DetachVolume actually removes an entry does that VM's *remaining*
// attachments stop matching their legacy name (by construction, since the
// index shifted) and fall through to volumeAttachmentName on the next
// reconcile -- a one-time, self-triggered, self-limited migration exactly
// when it's needed and never before.
func legacyVolumeAttachmentName(vmID string, index int) string {
	return fmt.Sprintf("volattach-%s-%d", vmID, index)
}

// existingVolumeAttachmentsByName fetches every VolumeAttachment for
// tenantID once and indexes it by Meta.Name, so createVolumeAttachments can
// check both the legacy and current naming scheme for every requested
// Volume with a single List call rather than one per Volume.
// VolumeAttachmentServiceClient has no lookup-by-name RPC (only
// Create/Get/List/Delete/Watch), so this is the same "List + client-side
// filter" tolerance hasActiveAttachment's own full scan already accepts at
// this system's target scale.
func existingVolumeAttachmentsByName(ctx context.Context, client blockstoragev1.VolumeAttachmentServiceClient, tenantID string) (map[string]*blockstoragev1.VolumeAttachment, error) {
	resp, err := client.List(ctx, &blockstoragev1.ListVolumeAttachmentsRequest{TenantId: tenantID})
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*blockstoragev1.VolumeAttachment, len(resp.GetItems()))
	for _, a := range resp.GetItems() {
		byName[a.GetMeta().GetName()] = a
	}
	return byName, nil
}

// createVolumeAttachments creates one VolumeAttachment per requested
// Volume, mirroring createNetworkInterfaces. refs lists every attachment
// created regardless of outcome (for
// VirtualMachineStatus.VolumeAttachmentRefs); infos includes only the ones
// that actually reached Attached -- one that came back Pending (blocked by
// the exclusive-attach constraint, docs/architecture.md「具体的な排他制御」)
// is left out, same "boot degrades gracefully rather than blocking"
// tolerance as an unresolved NetworkInterface IP. Attach-before-boot only
// (see docs/specs/volume.md): a VolumeAttachment that doesn't resolve by
// the time this runs simply isn't attached for this boot. This runs on
// every PhaseScheduled/PhaseStarting pass (fresh Create and Start-after-
// Stop alike, reconciler.go's provisionAndPublish), so Service.AttachVolume/
// DetachVolume mutating vm.Spec.Volumes while Stopped takes effect exactly
// here on the next Start -- no VMM-driver/reconciler changes needed.
//
// The protocol/connection/identifier compute-agent's volumeref needs don't
// live on the attachment itself (VolumeAttachmentStatus only carries
// phase/device_path/hypervisor -- see docs/architecture.md「訂正: 責務の境界
// を...」) but on the Volume it points at, so this also fetches that Volume
// via volumeClient. validateVolumes already confirmed it exists and is
// Ready moments ago at Create time, but re-fetching here (rather than
// threading that earlier result through) matches this function's own
// "re-derive at scheduling time" stance on freshness, same as
// createNetworkInterfaces re-deriving the network zone.
func createVolumeAttachments(ctx context.Context, volumeClient blockstoragev1.VolumeServiceClient, attachmentClient blockstoragev1.VolumeAttachmentServiceClient, tenantID, vmID string, volumes []VolumeRequest) (infos []VolumeAttachInfo, refs []string, err error) {
	byName, err := existingVolumeAttachmentsByName(ctx, attachmentClient, tenantID)
	if err != nil {
		return nil, nil, err
	}

	for i, v := range volumes {
		a := byName[legacyVolumeAttachmentName(vmID, i)]
		if a == nil || a.GetSpec().GetVolumeId() != v.VolumeID {
			a = byName[volumeAttachmentName(vmID, v.VolumeID)]
		}
		if a == nil {
			a, err = attachmentClient.Create(ctx, &blockstoragev1.CreateVolumeAttachmentRequest{
				TenantId: tenantID,
				Name:     volumeAttachmentName(vmID, v.VolumeID),
				Spec: &blockstoragev1.VolumeAttachmentSpec{
					VolumeId:   v.VolumeID,
					VmId:       vmID,
					DeviceHint: v.DeviceHint,
				},
			})
			if err != nil {
				return infos, refs, err
			}
		}
		refs = append(refs, a.GetMeta().GetId())
		if a.GetStatus().GetPhase() == "Attached" {
			vol, err := volumeClient.Get(ctx, &blockstoragev1.GetVolumeRequest{TenantId: tenantID, Id: v.VolumeID})
			if err != nil {
				return infos, refs, err
			}
			infos = append(infos, VolumeAttachInfo{
				AttachmentID:      a.GetMeta().GetId(),
				Protocol:          vol.GetSpec().GetProtocol().String(),
				StorageConnection: vol.GetSpec().GetStorageConnection(),
				Identifier:        vol.GetSpec().GetIdentifier(),
				SizeGB:            vol.GetSpec().GetSizeGb(),
			})
		}
	}
	return infos, refs, nil
}
