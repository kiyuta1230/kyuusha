package compute

import (
	"context"
	"fmt"
	"net"
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
// VirtualMachine" rule as validateImage: a pinned Subnet must exist, be
// usable by tenantID, Ready and not being deleted; a Network likewise
// (minus Ready: its Subnets may still be coming). It also settles the VM's
// one zone ("マルチAZにまたがるVirtualMachineは作れない"): vmZone if
// given, else the pinned Subnets' zone -- which must all agree -- and an
// attachment naming only a Network needs one of the two. Returns that
// zone ("" only when there are no attachments at all).
func (s *Service) validateNetworkInterfaces(ctx context.Context, tenantID, vmZone string, attachments []NetworkAttachment) (zone string, err error) {
	zone = vmZone
	needZone := false
	for _, a := range attachments {
		switch {
		case a.SubnetID != "":
			sn, err := s.subnetClient.Get(ctx, &networkv1.GetSubnetRequest{TenantId: tenantID, Id: a.SubnetID})
			if err != nil {
				if status.Code(err) == codes.NotFound {
					return "", fmt.Errorf("%w: subnet_id %q does not exist", ErrValidation, a.SubnetID)
				}
				return "", err
			}
			if sn.GetStatus().GetPhase() != "Ready" {
				return "", fmt.Errorf("%w: subnet %q is not Ready (phase=%s)", ErrValidation, a.SubnetID, sn.GetStatus().GetPhase())
			}
			if sn.GetMeta().GetDeletedAt() != nil {
				return "", fmt.Errorf("%w: subnet %q is being deleted", ErrValidation, a.SubnetID)
			}
			if a.NetworkID != "" && a.NetworkID != sn.GetSpec().GetNetworkId() {
				return "", fmt.Errorf("%w: subnet %q is not in network %q", ErrValidation, a.SubnetID, a.NetworkID)
			}
			subnetZone := sn.GetSpec().GetZone()
			if zone == "" {
				zone = subnetZone
			} else if zone != subnetZone {
				return "", fmt.Errorf("%w: subnet %q is in zone %q, not %q; a VirtualMachine cannot span zones", ErrValidation, a.SubnetID, subnetZone, zone)
			}
		case a.NetworkID != "":
			if s.NetworkClient == nil {
				return "", fmt.Errorf("%w: attaching by network_id needs compute's network client, which isn't configured", ErrValidation)
			}
			n, err := s.NetworkClient.Get(ctx, &networkv1.GetNetworkRequest{TenantId: tenantID, Id: a.NetworkID})
			if err != nil {
				if status.Code(err) == codes.NotFound {
					return "", fmt.Errorf("%w: network_id %q does not exist", ErrValidation, a.NetworkID)
				}
				return "", err
			}
			if n.GetMeta().GetDeletedAt() != nil {
				return "", fmt.Errorf("%w: network %q is being deleted", ErrValidation, a.NetworkID)
			}
			needZone = true
		default:
			return "", fmt.Errorf("%w: network_interfaces[] needs network_id (or subnet_id)", ErrValidation)
		}
	}
	if needZone && zone == "" {
		return "", fmt.Errorf("%w: spec.zone is required when a network interface names only a Network", ErrValidation)
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
// Each result also carries what compute-agent needs to wire a real tap
// without a network client of its own (same shape as Image resolution for
// boot inputs): the CIDR/gateway of the Subnet the address landed on (the
// network service picks it for a Network-only attachment), and the
// Network/NetworkClass context VNAP/SNAP plugins get (AttachInfo).
//
// Create itself always returns a brand new NetworkInterface Pending, with
// no IP yet -- allocation happens asynchronously in network-reconciler
// (see internal/network/service.go's CreateNetworkInterface doc comment).
// waitForAllocation below gives it a short, bounded chance to finish before
// this VM's boot command is built; a NetworkInterface whose IP allocation
// still hasn't succeeded once that bound is hit (no Subnet with a free
// address, or just unlucky timing) is still returned (for
// VirtualMachineStatus.InterfaceRefs), just with IPAddress/CIDR left empty;
// compute-agent skips wiring it.
func (s *Service) createNetworkInterfaces(ctx context.Context, tenantID, vmID, zone string, attachments []NetworkAttachment) ([]NetworkInterfaceInfo, error) {
	infos := make([]NetworkInterfaceInfo, 0, len(attachments))
	for i, a := range attachments {
		spec := &networkv1.NetworkInterfaceSpec{VmId: vmID, SubnetId: a.SubnetID, NetworkId: a.NetworkID}
		if a.SubnetID == "" {
			spec.Zone = zone
		}
		n, err := s.netifClient.Create(ctx, &networkv1.CreateNetworkInterfaceRequest{
			TenantId: tenantID,
			Name:     fmt.Sprintf("iface-%s-%d", vmID, i),
			Spec:     spec,
		})
		if err != nil {
			return infos, err
		}
		n, err = waitForAllocation(ctx, s.netifClient, tenantID, n.GetMeta().GetId())
		if err != nil {
			return infos, err
		}
		info := NetworkInterfaceInfo{
			IfaceID:    n.GetMeta().GetId(),
			SubnetID:   n.GetStatus().GetSubnetId(),
			IPAddress:  n.GetStatus().GetIpAddress(),
			MACAddress: n.GetStatus().GetMacAddress(),
			Primary:    a.Primary,
			// Effective, not spec: includes any same-Network implicit
			// allow entries alongside what the tenant actually declared --
			// see NetworkInterfaceStatus's own doc comment in the proto.
			IngressRules: toFirewallRuleInfos(n.GetStatus().GetEffectiveIngressRules()),
			EgressRules:  toFirewallRuleInfos(n.GetStatus().GetEffectiveEgressRules()),
		}
		if info.IPAddress != "" && info.SubnetID != "" {
			sn, err := s.subnetClient.Get(ctx, &networkv1.GetSubnetRequest{TenantId: tenantID, Id: info.SubnetID})
			if err != nil {
				return infos, err
			}
			info.Zone = sn.GetSpec().GetZone()
			info.SubnetLabels = sn.GetMeta().GetLabels()
			for _, addr := range sn.GetStatus().GetAddresses() {
				if ip, _, err := net.ParseCIDR(addr.GetCidr()); err == nil && ip.To4() != nil {
					info.CIDR, info.GatewayIP = addr.GetCidr(), addr.GetGatewayIp()
				}
			}
			info.Attach = s.attachInfo(ctx, tenantID, sn)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// attachInfo resolves sn's Network and NetworkClass into the plugin-facing
// AttachInfo. Best effort: a lookup failure leaves the corresponding
// fields empty rather than failing the boot.
func (s *Service) attachInfo(ctx context.Context, tenantID string, sn *networkv1.Subnet) AttachInfo {
	out := AttachInfo{
		NetworkID:        sn.GetSpec().GetNetworkId(),
		SubnetValues:     sn.GetStatus().GetValues(),
		SubnetAttributes: sn.GetStatus().GetAttributes(),
	}
	if s.NetworkClient == nil {
		return out
	}
	n, err := s.NetworkClient.Get(ctx, &networkv1.GetNetworkRequest{TenantId: tenantID, Id: out.NetworkID})
	if err != nil {
		return out
	}
	out.NetworkLabels, out.NetworkValues, out.NetworkAttributes = n.GetMeta().GetLabels(), n.GetStatus().GetValues(), n.GetStatus().GetAttributes()
	if s.NetworkClassClient == nil {
		return out
	}
	if c, err := s.NetworkClassClient.Get(ctx, &networkv1.GetNetworkClassRequest{Id: n.GetSpec().GetNetworkClass()}); err == nil {
		out.NetworkClass, out.NetworkClassAttributes, out.MTU = c.GetMeta().GetName(), c.GetSpec().GetAttributes(), c.GetSpec().GetMtu()
	}
	return out
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
