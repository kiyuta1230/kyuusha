package compute

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// validateNetworkInterfaces implements docs/architecture.md's Create-time
// validation of spec.network_interfaces, the same "never create a doomed
// VirtualMachine" rule as validateImage: every referenced Subnet must
// exist, belong to tenantID, and be Ready. It also enforces "マルチAZに
// またがるVirtualMachineは作れない" -- all referenced Subnets must share one
// zone -- since a VirtualMachine only ever runs on a single Hypervisor.
// Returns that shared zone (empty if attachments is empty: no constraint,
// this VM just has no network yet -- see docs/specs/network.md's still-
// missing tap wiring).
func validateNetworkInterfaces(ctx context.Context, client networkv1.SubnetServiceClient, tenantID string, attachments []NetworkAttachment) (zone string, err error) {
	for _, a := range attachments {
		if a.SubnetID == "" {
			return "", fmt.Errorf("%w: network_interfaces[].subnet_id is required", ErrValidation)
		}
		sn, err := client.Get(ctx, &networkv1.GetSubnetRequest{TenantId: tenantID, Id: a.SubnetID})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return "", fmt.Errorf("%w: subnet_id %q does not exist", ErrValidation, a.SubnetID)
			}
			return "", err
		}
		if sn.GetStatus().GetPhase() != "Ready" {
			return "", fmt.Errorf("%w: subnet %q is not Ready (phase=%s)", ErrValidation, a.SubnetID, sn.GetStatus().GetPhase())
		}
		subnetZone := sn.GetSpec().GetZone()
		if zone == "" {
			zone = subnetZone
		} else if zone != subnetZone {
			return "", fmt.Errorf("%w: network_interfaces reference Subnets in different zones (%q and %q); a VirtualMachine cannot span zones", ErrValidation, zone, subnetZone)
		}
	}
	return zone, nil
}

// createNetworkInterfaces creates one NetworkInterface per attachment,
// named deterministically (docs/architecture.md's "子リソースIDの決定的生成
// ルール": iface-<vm-id>-<index>) so a retry of this step (e.g. after the
// Reconciler's own Update following this call fails and the VM is
// re-observed still Scheduled) is idempotent via network's own
// idempotent-by-name Create, not by anything special here. Returns the
// created NetworkInterface IDs, for VirtualMachineStatus.InterfaceRefs.
func createNetworkInterfaces(ctx context.Context, client networkv1.NetworkInterfaceServiceClient, tenantID, vmID string, attachments []NetworkAttachment) ([]string, error) {
	refs := make([]string, 0, len(attachments))
	for i, a := range attachments {
		n, err := client.Create(ctx, &networkv1.CreateNetworkInterfaceRequest{
			TenantId: tenantID,
			Name:     fmt.Sprintf("iface-%s-%d", vmID, i),
			Spec: &networkv1.NetworkInterfaceSpec{
				VmId:     vmID,
				SubnetId: a.SubnetID,
			},
		})
		if err != nil {
			return refs, err
		}
		refs = append(refs, n.GetMeta().GetId())
	}
	return refs, nil
}
