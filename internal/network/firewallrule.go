package network

import (
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
