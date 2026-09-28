package network

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// validateFirewallRules is shared by ingress_rules and egress_rules --
// direction doesn't change the shape of a single rule, only which nftables
// chain compute-agent's SNAP/nftacl enforces it in (see docs/specs/snap.md).
func validateFirewallRules(rules []FirewallRule) error {
	for i, r := range rules {
		switch r.Protocol {
		case "tcp", "udp", "icmp":
		default:
			return fmt.Errorf("%w: ingress_rules[%d].protocol %q must be tcp/udp/icmp", ErrValidation, i, r.Protocol)
		}
		switch r.Action {
		case "allow", "deny":
		default:
			return fmt.Errorf("%w: ingress_rules[%d].action %q must be allow/deny", ErrValidation, i, r.Action)
		}
		if _, _, err := net.ParseCIDR(r.SourceCIDR); err != nil {
			return fmt.Errorf("%w: ingress_rules[%d].source_cidr %q: %v", ErrValidation, i, r.SourceCIDR, err)
		}
		if r.Protocol != "icmp" {
			if err := validatePortRange(r.PortRange); err != nil {
				return fmt.Errorf("%w: ingress_rules[%d].port_range %q: %v", ErrValidation, i, r.PortRange, err)
			}
		}
	}
	return nil
}

// cidrsOverlap reports whether a and b's ranges intersect: either CIDR
// contains the other's network address, not exact equality, so a Subnet
// can't dodge validateUniqueCIDR by naming a slightly different but
// still-overlapping range. Formerly also used by validateCrossTenantRules
// (removed -- see SubnetSpec.shared_with_tenant_ids's proto comment: an
// equivalent or stronger cross-tenant ACL policy is available by wiring
// internal/admissionwebhook into this service instead).
func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// EffectiveFirewallRules is spec.ingress_rules/egress_rules plus, when n's
// Subnet declares a non-empty mesh_group, an implicit allow entry for every
// other Subnet in the same tenant sharing that mesh_group (see
// docs/specs/network.md「spec.mesh_group」) -- this is what actually gets
// enforced on the host, not necessarily what's stored in spec (mesh_group-
// derived entries are never written back to etcd, same "implicit baseline,
// not persisted" treatment as SubnetCIDR/GatewayIP already gets in
// nftacl/SNAP). protocol="" on a synthetic entry means "any protocol",
// matching nftacl/SNAP's existing protocol==0-is-any convention.
func (s *Service) EffectiveFirewallRules(ctx context.Context, n *NetworkInterface) (ingress, egress []FirewallRule, err error) {
	ingress = n.Spec.IngressRules
	egress = n.Spec.EgressRules

	subnet, err := s.getSubnetForInterface(ctx, n.Meta.TenantID, n.Spec.SubnetID)
	if err != nil {
		return nil, nil, err
	}
	if subnet.Spec.MeshGroup == "" {
		return ingress, egress, nil
	}

	siblings, err := s.subnets.List(ctx, n.Meta.TenantID)
	if err != nil {
		return nil, nil, err
	}
	for _, sib := range siblings {
		if sib.Meta.ID == subnet.Meta.ID || sib.Spec.MeshGroup != subnet.Spec.MeshGroup {
			continue
		}
		meshRule := FirewallRule{SourceCIDR: sib.Spec.CIDR, Action: "allow"}
		ingress = append(append([]FirewallRule{}, ingress...), meshRule)
		egress = append(append([]FirewallRule{}, egress...), meshRule)
	}
	return ingress, egress, nil
}

// validatePortRange accepts a single port ("22") or an inclusive range
// ("2379-2380").
func validatePortRange(s string) error {
	parsePort := func(p string) (int, error) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("port must be 1-65535")
		}
		return n, nil
	}
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		_, err := parsePort(s)
		return err
	}
	loN, err := parsePort(lo)
	if err != nil {
		return err
	}
	hiN, err := parsePort(hi)
	if err != nil {
		return err
	}
	if loN > hiN {
		return fmt.Errorf("range start %d exceeds end %d", loN, hiN)
	}
	return nil
}
