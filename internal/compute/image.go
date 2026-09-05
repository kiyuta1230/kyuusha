package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	imagev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

// validateImage implements docs/architecture.md's Create-time validation
// ("imageサービスのリソース: Image"): the referenced Image must exist, be
// Ready (not still Pending or Error -- a doomed VirtualMachine is never
// created just to be marked Error afterwards, same reasoning as Quota),
// and its format must match the VMM the VM itself requests
// (KERNEL_ROOTFS<->FIRECRACKER, QCOW2<->QEMU). Format mismatches are never
// auto-transcoded (see the doc); they're a synchronous Create-time reject.
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
	var wantDriver VmmDriver
	switch format {
	case imagev1.ImageFormat_KERNEL_ROOTFS:
		wantDriver = VmmDriverFirecracker
	case imagev1.ImageFormat_QCOW2:
		wantDriver = VmmDriverQEMU
	default:
		return fmt.Errorf("%w: image %q has no usable format", ErrValidation, imageID)
	}
	if driver != wantDriver {
		return fmt.Errorf("%w: image %q format %s requires driver_hint %s, got %s", ErrValidation, imageID, format, wantDriver, driver)
	}
	return nil
}
