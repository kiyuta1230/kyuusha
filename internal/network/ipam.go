package network

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
)

// ipPool hands out exclusive IPv4 addresses per Subnet, drawn from that
// Subnet's own spec.cidr (network/broadcast addresses and, if set,
// gateway_ip are never handed out, regardless of allocatableRanges below).
// IPv6 CIDRs and /31,/32 (no usable host range in this simple model) always
// report exhausted.
type ipPool struct {
	mu   sync.Mutex
	used map[string]map[string]bool // subnetID -> allocated IP strings
}

func newIPPool() *ipPool {
	return &ipPool{used: make(map[string]map[string]bool)}
}

// allocate draws from allocatableRanges if given (each already validated at
// Create time by validateAllocatableIPRanges, but re-clamped here to the
// CIDR's real usable host range defensively -- e.g. in case a Subnet's spec
// was mutated after Create without going back through that validation, see
// Service.UpdateSubnet), or the whole CIDR's usable host range otherwise.
func (p *ipPool) allocate(subnetID, cidr, gatewayIP string, allocatableRanges []string) (ip string, ok bool) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", false
	}
	hostStart, hostEnd, ok := hostRange(ipnet)
	if !ok {
		return "", false
	}

	ranges := []ipRange{{hostStart, hostEnd}}
	if len(allocatableRanges) > 0 {
		ranges = ranges[:0]
		for _, r := range allocatableRanges {
			start, end, err := parseIPRange(r)
			if err != nil {
				continue // already validated at Create time; defensively skip
			}
			if ipAfter(hostStart, start) {
				start = hostStart
			}
			if ipAfter(end, hostEnd) {
				end = hostEnd
			}
			if ipAfter(start, end) {
				continue // this range doesn't overlap the usable host range at all
			}
			ranges = append(ranges, ipRange{start, end})
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	subnetUsed := p.used[subnetID]
	if subnetUsed == nil {
		subnetUsed = make(map[string]bool)
		p.used[subnetID] = subnetUsed
	}
	for _, rg := range ranges {
		for cur := rg.start; !ipAfter(cur, rg.end); cur = nextIP(cur) {
			s := cur.String()
			if s == gatewayIP || subnetUsed[s] {
				continue
			}
			subnetUsed[s] = true
			return s, true
		}
	}
	return "", false
}

type ipRange struct{ start, end net.IP }

// parseIPRange parses "<start-ip>-<end-ip>" (both IPv4) into its inclusive
// bounds.
func parseIPRange(s string) (start, end net.IP, err error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("expected \"<start-ip>-<end-ip>\", got %q", s)
	}
	start = net.ParseIP(strings.TrimSpace(parts[0])).To4()
	end = net.ParseIP(strings.TrimSpace(parts[1])).To4()
	if start == nil || end == nil {
		return nil, nil, fmt.Errorf("invalid IPv4 address in range %q", s)
	}
	if ipAfter(start, end) {
		return nil, nil, fmt.Errorf("range start is after its end in %q", s)
	}
	return start, end, nil
}

func (p *ipPool) release(subnetID, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used[subnetID], ip)
}

// markUsed records ip as already allocated in subnetID without drawing a
// new one -- see vlanPool.markUsed's identical reasoning.
func (p *ipPool) markUsed(subnetID, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used[subnetID] == nil {
		p.used[subnetID] = make(map[string]bool)
	}
	p.used[subnetID][ip] = true
}

// validateAllocatableIPRanges checks each range parses and falls entirely
// within cidr -- called at Subnet Create time so a typo'd range (e.g. from
// the wrong subnet entirely) is rejected up front rather than silently
// shrinking the pool to nothing.
func validateAllocatableIPRanges(cidr string, ranges []string) error {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return err // CIDR itself is validated by the caller first
	}
	for _, r := range ranges {
		start, end, err := parseIPRange(r)
		if err != nil {
			return err
		}
		if !ipnet.Contains(start) || !ipnet.Contains(end) {
			return fmt.Errorf("range %q is outside %s", r, cidr)
		}
	}
	return nil
}

// hostRange returns the usable host address range for ipnet (network and
// broadcast address excluded), or ok=false for anything this simple IPv4-
// only model doesn't support.
func hostRange(ipnet *net.IPNet) (start, end net.IP, ok bool) {
	ip4 := ipnet.IP.To4()
	mask4 := net.IP(ipnet.Mask).To4()
	if ip4 == nil || mask4 == nil {
		return nil, nil, false // IPv6 not supported
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 || ones >= 31 {
		return nil, nil, false // /31,/32: no usable host range here
	}

	network := binary.BigEndian.Uint32(ip4)
	mask := binary.BigEndian.Uint32(mask4)
	broadcast := network | ^mask
	return uint32ToIP(network + 1), uint32ToIP(broadcast - 1), true
}

func uint32ToIP(v uint32) net.IP {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func nextIP(ip net.IP) net.IP {
	return uint32ToIP(binary.BigEndian.Uint32(ip.To4()) + 1)
}

func ipAfter(a, b net.IP) bool {
	return binary.BigEndian.Uint32(a.To4()) > binary.BigEndian.Uint32(b.To4())
}
