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

// createVolumeAttachments creates one VolumeAttachment per requested
// Volume, named deterministically (docs/architecture.md's "子リソースIDの
// 決定的生成ルール": volattach-<vm-id>-<index>), mirroring
// createNetworkInterfaces. refs lists every attachment created regardless
// of outcome (for VirtualMachineStatus.VolumeAttachmentRefs); infos
// includes only the ones that actually reached Attached -- one that came
// back Pending (blocked by the exclusive-attach constraint,
// docs/architecture.md「具体的な排他制御」) is left out, same "boot degrades
// gracefully rather than blocking" tolerance as an unresolved
// NetworkInterface IP. Attach-before-boot only (see docs/specs/volume.md):
// a VolumeAttachment that doesn't resolve by the time this runs simply
// isn't attached for this boot -- there is no later retry/hot-plug path in
// v1.
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
	for i, v := range volumes {
		a, err := attachmentClient.Create(ctx, &blockstoragev1.CreateVolumeAttachmentRequest{
			TenantId: tenantID,
			Name:     fmt.Sprintf("volattach-%s-%d", vmID, i),
			Spec: &blockstoragev1.VolumeAttachmentSpec{
				VolumeId:   v.VolumeID,
				VmId:       vmID,
				DeviceHint: v.DeviceHint,
			},
		})
		if err != nil {
			return infos, refs, err
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
