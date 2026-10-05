package network

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

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
