package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

// validateImage implements docs/architecture.md's Create-time validation
// ("imageサービスのリソース: Image"): the referenced Image must exist, be
// Ready (not still Pending or Error -- a doomed VirtualMachine is never
// created just to be marked Error afterwards, same reasoning as Quota),
// and its format must be usable by the VMM the VM itself requests. Format
// mismatches are never auto-transcoded (see the doc); they're a
// synchronous Create-time reject.
//
// KERNEL_ROOTFS pairs with either FIRECRACKER or CLOUD_HYPERVISOR: both
// drivers boot the identical asset (a kernel + a raw rootfs, no bootloader)
// via their own direct-kernel-boot mechanism -- see docs/specs/
// firecracker-boot.md and docs/specs/cloud-hypervisor-boot.md. QCOW2 pairs
// with CLOUD_HYPERVISOR only, and remains unconsumed by any driver today
// (reserved for a future self-contained bootable-disk boot path -- e.g.
// non-Linux guests -- that neither driver implements yet; see
// docs/specs/cloud-hypervisor-boot.md's known gaps).
func validateImage(ctx context.Context, client imagev1.ImageServiceClient, tenantID, imageID string, driver VmmDriver) error {
	img, err := client.Get(ctx, &imagev1.GetImageRequest{TenantId: tenantID, Id: imageID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return fmt.Errorf("%w: image_id %q does not exist", ErrValidation, imageID)
		}
		return err
	}
	if img.GetStatus().GetPhase() != "Ready" {
		return fmt.Errorf("%w: image %q is not Ready (phase=%s)", ErrValidation, imageID, img.GetStatus().GetPhase())
	}

	format := img.GetSpec().GetFormat()
	switch format {
	case imagev1.ImageFormat_KERNEL_ROOTFS:
		if driver != VmmDriverFirecracker && driver != VmmDriverCloudHypervisor {
			return fmt.Errorf("%w: image %q format %s requires driver_hint %s or %s, got %s",
				ErrValidation, imageID, format, VmmDriverFirecracker, VmmDriverCloudHypervisor, driver)
		}
	case imagev1.ImageFormat_QCOW2:
		if driver != VmmDriverCloudHypervisor {
			return fmt.Errorf("%w: image %q format %s requires driver_hint %s, got %s", ErrValidation, imageID, format, VmmDriverCloudHypervisor, driver)
		}
	default:
		return fmt.Errorf("%w: image %q has no usable format", ErrValidation, imageID)
	}
	return nil
}
