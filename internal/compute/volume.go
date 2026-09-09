package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	blockstoragev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
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
// includes only the ones that actually reached Attached with real iSCSI
// connection info -- one that came back Pending (blocked by the exclusive-
// attach constraint, docs/architecture.md「具体的な排他制御」) or Error
// (storage backend failure) is left out, same "boot degrades gracefully
// rather than blocking" tolerance as an unresolved NetworkInterface IP.
// Attach-before-boot only (see docs/specs/volume.md): a VolumeAttachment
// that doesn't resolve by the time this runs simply isn't attached for
// this boot -- there is no later retry/hot-plug path in v1.
func createVolumeAttachments(ctx context.Context, client blockstoragev1.VolumeAttachmentServiceClient, tenantID, vmID string, volumes []VolumeRequest) (infos []VolumeAttachInfo, refs []string, err error) {
	for i, v := range volumes {
		a, err := client.Create(ctx, &blockstoragev1.CreateVolumeAttachmentRequest{
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
		if a.GetStatus().GetPhase() == "Attached" && a.GetStatus().GetTargetIqn() != "" {
			infos = append(infos, VolumeAttachInfo{
				AttachmentID: a.GetMeta().GetId(),
				TargetIQN:    a.GetStatus().GetTargetIqn(),
				TargetPortal: a.GetStatus().GetTargetPortal(),
			})
		}
	}
	return infos, refs, nil
}
