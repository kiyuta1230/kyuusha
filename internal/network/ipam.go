package network

import (
	"encoding/binary"
	"net"
	"sync"
)

// minVLANID/maxVLANID: VLAN 0 and 4095 are reserved, so the usable ID space
// is 1-4094 (docs/architecture.md's "4094の上限"). Pools are independent per
// zone -- the same VLAN number is reusable across zones.
const (
	minVLANID = 1
	maxVLANID = 4094
)

// vlanPool hands out exclusive VLAN IDs per zone. Allocation/release are
// synchronous, in-memory, no agent involvement -- see
// docs/architecture.md's "VLAN IDの払い出し".
type vlanPool struct {
	mu   sync.Mutex
	used map[string]map[int32]bool // zone -> allocated ids
}

func newVLANPool() *vlanPool {
	return &vlanPool{used: make(map[string]map[int32]bool)}
}

// allocate returns the lowest free VLAN ID in zone, or ok=false if the
// zone's pool is exhausted (all of 1-4094 in use).
func (p *vlanPool) allocate(zone string) (id int32, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	zoneUsed := p.used[zone]
	if zoneUsed == nil {
		zoneUsed = make(map[int32]bool)
		p.used[zone] = zoneUsed
	}
	for id := int32(minVLANID); id <= maxVLANID; id++ {
		if !zoneUsed[id] {
			zoneUsed[id] = true
			return id, true
		}
	}
	return 0, false
}

func (p *vlanPool) release(zone string, id int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used[zone], id)
}

// ipPool hands out exclusive IPv4 addresses per Subnet, drawn from that
// Subnet's own spec.cidr (network/broadcast addresses and, if set,
// gateway_ip are never handed out). IPv6 CIDRs and /31,/32 (no usable host
// range in this simple model) always report exhausted.
type ipPool struct {
	mu   sync.Mutex
	used map[string]map[string]bool // subnetID -> allocated IP strings
}

func newIPPool() *ipPool {
	return &ipPool{used: make(map[string]map[string]bool)}
}

func (p *ipPool) allocate(subnetID, cidr, gatewayIP string) (ip string, ok bool) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", false
	}
	start, end, ok := hostRange(ipnet)
	if !ok {
		return "", false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	subnetUsed := p.used[subnetID]
	if subnetUsed == nil {
		subnetUsed = make(map[string]bool)
		p.used[subnetID] = subnetUsed
	}
	for cur := start; !ipAfter(cur, end); cur = nextIP(cur) {
		s := cur.String()
		if s == gatewayIP || subnetUsed[s] {
			continue
		}
		subnetUsed[s] = true
		return s, true
	}
	return "", false
}

func (p *ipPool) release(subnetID, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used[subnetID], ip)
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
