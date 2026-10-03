package compute

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// netifAllocationPollInterval/netifAllocationPollTimeout bound how long
// createNetworkInterfaces waits for a freshly-created NetworkInterface's IP
// (network-reconciler's own async allocation, see internal/network/
// service.go's CreateNetworkInterface: "Always created Pending, with no
// mac_address/ip_address set yet") before giving up and booting without
// it. This runs inside compute.Reconciler.Run's single serialized Watch
// loop (see reconciler.go), so the bound must stay short: network's own
// watchPendingNetworkInterfaces reacts to the Create as an EventAdded
// near-instantly in the common case (this is normally sub-second), and a
// genuinely exhausted IP pool will still be Pending when the timeout hits,
// which resolves to the pre-existing "boot without this interface,
// compute-agent skips wiring it" behavior -- not a new failure mode, just
// reached slightly later than before this poll existed.
// Package vars, not consts, so tests can shrink them (see network_test.go)
// instead of a real test taking netifAllocationPollTimeout to exercise the
// still-Pending-at-timeout path.
var (
	netifAllocationPollInterval = 100 * time.Millisecond
	netifAllocationPollTimeout  = 3 * time.Second
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
// idempotent-by-name Create, not by anything special here.
//
// Each result also carries its Subnet's CIDR/gateway_ip/vlan_id (re-fetched
// here rather than reusing whatever validateNetworkInterfaces saw moments
// earlier -- Subnets can change between Create and Scheduled, same
// reasoning as the zone re-derivation in reconciler.go), so compute-agent
// has everything it needs to wire a real tap device (see
// internal/compute-agent/netsetup) without a network service client of its
// own -- same shape as Image resolution for boot inputs.
//
// Create itself always returns a brand new NetworkInterface Pending, with
// no IP yet -- allocation happens asynchronously in network-reconciler
// (see internal/network/service.go's CreateNetworkInterface doc comment).
// waitForAllocation below gives it a short, bounded chance to finish before
// this VM's boot command is built; a NetworkInterface whose IP allocation
// still hasn't succeeded once that bound is hit (a genuinely exhausted
// Subnet pool, or just unlucky timing) is still returned (for
// VirtualMachineStatus.InterfaceRefs), just with IPAddress/CIDR left empty;
// compute-agent skips wiring it.
func createNetworkInterfaces(ctx context.Context, subnetClient networkv1.SubnetServiceClient, netifClient networkv1.NetworkInterfaceServiceClient, tenantID, vmID string, attachments []NetworkAttachment) ([]NetworkInterfaceInfo, error) {
	infos := make([]NetworkInterfaceInfo, 0, len(attachments))
	for i, a := range attachments {
		n, err := netifClient.Create(ctx, &networkv1.CreateNetworkInterfaceRequest{
			TenantId: tenantID,
			Name:     fmt.Sprintf("iface-%s-%d", vmID, i),
			Spec: &networkv1.NetworkInterfaceSpec{
				VmId:     vmID,
				SubnetId: a.SubnetID,
			},
		})
		if err != nil {
			return infos, err
		}
		n, err = waitForAllocation(ctx, netifClient, tenantID, n.GetMeta().GetId())
		if err != nil {
			return infos, err
		}
		info := NetworkInterfaceInfo{
			IfaceID:    n.GetMeta().GetId(),
			SubnetID:   a.SubnetID,
			IPAddress:  n.GetStatus().GetIpAddress(),
			MACAddress: n.GetStatus().GetMacAddress(),
			Primary:    a.Primary,
			// Effective, not spec: includes any mesh_group-derived implicit
			// allow entries alongside what the tenant actually declared --
			// see NetworkInterfaceStatus's own doc comment in the proto.
			// Create/Get (waitForAllocation polls via Get) both populate
			// this.
			IngressRules: toFirewallRuleInfos(n.GetStatus().GetEffectiveIngressRules()),
			EgressRules:  toFirewallRuleInfos(n.GetStatus().GetEffectiveEgressRules()),
		}
		if info.IPAddress != "" {
			sn, err := subnetClient.Get(ctx, &networkv1.GetSubnetRequest{TenantId: tenantID, Id: a.SubnetID})
			if err != nil {
				return infos, err
			}
			info.Zone = sn.GetSpec().GetZone()
			info.CIDR = sn.GetSpec().GetCidr()
			info.GatewayIP = sn.GetSpec().GetGatewayIp()
			info.VLANID = sn.GetStatus().GetVlanId()
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// waitForAllocation polls ifaceID until its IP allocation completes
// (phase leaves Pending, i.e. either Ready with an IP or -- not currently
// modeled as a distinct phase, but handled the same way -- still Pending
// past netifAllocationPollTimeout) or netifAllocationPollTimeout elapses,
// whichever first; returns n's latest known state either way, never an
// error just for still being Pending (see createNetworkInterfaces' doc
// comment on what an empty IPAddress means downstream).
func waitForAllocation(ctx context.Context, netifClient networkv1.NetworkInterfaceServiceClient, tenantID, ifaceID string) (*networkv1.NetworkInterface, error) {
	deadline := time.Now().Add(netifAllocationPollTimeout)
	for {
		n, err := netifClient.Get(ctx, &networkv1.GetNetworkInterfaceRequest{TenantId: tenantID, Id: ifaceID})
		if err != nil {
			return nil, err
		}
		if n.GetStatus().GetPhase() != "Pending" || time.Now().After(deadline) {
			return n, nil
		}
		select {
		case <-ctx.Done():
			return n, nil
		case <-time.After(netifAllocationPollInterval):
		}
	}
}

func toFirewallRuleInfos(rules []*networkv1.FirewallRule) []FirewallRuleInfo {
	var out []FirewallRuleInfo
	for _, r := range rules {
		out = append(out, FirewallRuleInfo{
			Protocol: r.GetProtocol(), PortRange: r.GetPortRange(),
			SourceCIDR: r.GetSourceCidr(), Action: r.GetAction(),
		})
	}
	return out
}
