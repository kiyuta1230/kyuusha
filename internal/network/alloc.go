package network

import (
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net"
	"slices"
	"sync"
)

// allocator is the in-memory bookkeeping of what has been drawn from every
// AllocationPool -- the generalization of the old per-zone VLAN pool. Like
// that pool it lives only in network-reconciler (the sole allocating
// process): rebuilt from every Network's/Subnet's status.allocations at
// startup, marked on allocation, released on the owner's Deleted event.
type allocator struct {
	mu      sync.Mutex
	ints    map[string]map[int64]bool  // pool id -> integers in use
	entries map[string]map[string]bool // pool id -> entry keys in use
	cidrs   map[string]map[string]bool // pool id -> CIDRs in use
}

func newAllocator() *allocator {
	return &allocator{ints: map[string]map[int64]bool{}, entries: map[string]map[string]bool{}, cidrs: map[string]map[string]bool{}}
}

// errAllocPending means "not now": the pool is exhausted or the request
// can't be satisfied as things stand. The owner stays Pending with the
// message in a Condition and is retried by the periodic sweep.
var errAllocPending = errors.New("allocation pending")

func (a *allocator) markUsed(al Allocation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.markLocked(al)
}

func (a *allocator) markLocked(al Allocation) {
	switch {
	case al.EntryKey != "":
		setAdd(a.entries, al.PoolID, al.EntryKey)
	case al.CIDR != "":
		setAdd(a.cidrs, al.PoolID, al.CIDR)
	default:
		setAdd(a.ints, al.PoolID, al.Integer)
	}
}

func (a *allocator) release(als []Allocation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseLocked(als)
}

func (a *allocator) releaseLocked(als []Allocation) {
	for _, al := range als {
		switch {
		case al.EntryKey != "":
			delete(a.entries[al.PoolID], al.EntryKey)
		case al.CIDR != "":
			delete(a.cidrs[al.PoolID], al.CIDR)
		default:
			delete(a.ints[al.PoolID], al.Integer)
		}
	}
}

func setAdd[K comparable](m map[string]map[K]bool, pool string, k K) {
	if m[pool] == nil {
		m[pool] = map[K]bool{}
	}
	m[pool][k] = true
}

// allocRequest is what an allocation is for.
type allocRequest struct {
	requested []SubnetAddress  // the Subnet's spec.requested_addresses (user-specified CIDR modes)
	avoid     []*net.IPNet     // CIDRs already used inside the same Network
	placement GatewayPlacement // how to derive a gateway the pool/request didn't give
}

// allocResult is what an allocation produced.
type allocResult struct {
	values      map[string]int64
	attributes  map[string]string
	addresses   []SubnetAddress
	allocations []Allocation
}

// allocateAll draws one allocation per ref, all or nothing: on any failure
// everything taken so far is returned to the pools.
func (a *allocator) allocateAll(pools map[string]AllocationPool, refs []PoolRef, req allocRequest) (allocResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	res := allocResult{values: map[string]int64{}, attributes: map[string]string{}}
	for _, ref := range refs {
		pool, ok := pools[ref.PoolID]
		if !ok {
			a.releaseLocked(res.allocations)
			return allocResult{}, fmt.Errorf("%w: pool %q does not exist", errAllocPending, ref.PoolID)
		}
		if err := a.allocateOneLocked(pool, ref, req, &res); err != nil {
			a.releaseLocked(res.allocations)
			return allocResult{}, err
		}
		maps.Copy(res.attributes, pool.Spec.Attributes)
	}
	return res, nil
}

func (a *allocator) allocateOneLocked(pool AllocationPool, ref PoolRef, req allocRequest, res *allocResult) error {
	id := pool.Meta.ID
	switch pool.Spec.Kind() {
	case PoolKindInteger:
		for _, r := range pool.Spec.Integer {
			for v := r.Lo; v <= r.Hi; v++ {
				if !a.ints[id][v] {
					al := Allocation{PoolID: id, Name: ref.Name, Integer: v}
					a.markLocked(al)
					res.allocations = append(res.allocations, al)
					res.values[ref.Name] = v
					return nil
				}
			}
		}
		return fmt.Errorf("%w: integer pool %q is exhausted", errAllocPending, pool.Meta.Name)

	case PoolKindEntries:
		for _, e := range pool.Spec.Entries {
			if a.entries[id][e.Key] || overlapsAny(e.Addresses, req.avoid) {
				continue
			}
			al := Allocation{PoolID: id, EntryKey: e.Key}
			a.markLocked(al)
			res.allocations = append(res.allocations, al)
			maps.Copy(res.values, e.Values)
			maps.Copy(res.attributes, e.Attributes)
			for _, b := range e.Addresses {
				gw := b.GatewayIP
				if gw == "" {
					gw = gatewayFor(b.CIDR, req.placement)
				}
				res.addresses = append(res.addresses, SubnetAddress{CIDR: b.CIDR, GatewayIP: gw})
			}
			return nil
		}
		return fmt.Errorf("%w: entries pool %q has no free entry", errAllocPending, pool.Meta.Name)

	case PoolKindCidr:
		spec := *pool.Spec.Cidr
		family := spec.Family
		if family == "" {
			family = FamilyIPv4
		}
		var requested *SubnetAddress
		for i := range req.requested {
			if familyOf(req.requested[i].CIDR) == family {
				requested = &req.requested[i]
			}
		}
		var cidr string
		switch spec.Mode {
		case CidrModeUserAny, CidrModeUserWithinBlocks:
			if requested == nil {
				return fmt.Errorf("%w: pool %q needs a requested %s CIDR on the Subnet", errAllocPending, pool.Meta.Name, family)
			}
			_, want, err := net.ParseCIDR(requested.CIDR)
			if err != nil {
				return fmt.Errorf("%w: requested CIDR %q: %v", errAllocPending, requested.CIDR, err)
			}
			if spec.Mode == CidrModeUserWithinBlocks {
				if !insideAny(want, spec.Blocks) {
					return fmt.Errorf("%w: requested CIDR %s is outside pool %q's blocks", errAllocPending, want, pool.Meta.Name)
				}
				if a.overlapsUsedLocked(id, want) {
					return fmt.Errorf("%w: requested CIDR %s overlaps an address already allocated from pool %q", errAllocPending, want, pool.Meta.Name)
				}
			}
			cidr = want.String()
		case CidrModeCarve:
			carved, ok := a.carveLocked(id, spec, req.avoid)
			if !ok {
				return fmt.Errorf("%w: pool %q has no free /%d left", errAllocPending, pool.Meta.Name, spec.PrefixLength)
			}
			cidr = carved
		default:
			return fmt.Errorf("%w: pool %q has unknown CIDR mode %q", errAllocPending, pool.Meta.Name, spec.Mode)
		}
		gw := ""
		if requested != nil {
			gw = requested.GatewayIP
		}
		if gw == "" {
			gw = gatewayFor(cidr, req.placement)
		}
		al := Allocation{PoolID: id, CIDR: cidr}
		a.markLocked(al)
		res.allocations = append(res.allocations, al)
		res.addresses = append(res.addresses, SubnetAddress{CIDR: cidr, GatewayIP: gw})
		return nil
	}
	return fmt.Errorf("%w: pool %q has no kind", errAllocPending, pool.Meta.Name)
}

func (a *allocator) overlapsUsedLocked(pool string, n *net.IPNet) bool {
	for used := range a.cidrs[pool] {
		if _, u, err := net.ParseCIDR(used); err == nil && cidrsOverlap(u, n) {
			return true
		}
	}
	return false
}

// carveLocked returns the first /PrefixLength inside spec.Blocks that
// overlaps neither this pool's existing allocations nor avoid.
func (a *allocator) carveLocked(pool string, spec CidrPoolSpec, avoid []*net.IPNet) (string, bool) {
	for _, b := range spec.Blocks {
		_, block, err := net.ParseCIDR(b)
		if err != nil {
			continue
		}
		blockOnes, bits := block.Mask.Size()
		size := int(spec.PrefixLength)
		if size < blockOnes || size > bits {
			continue
		}
		step := new(big.Int).Lsh(big.NewInt(1), uint(bits-size))
		count := new(big.Int).Lsh(big.NewInt(1), uint(size-blockOnes))
		start := new(big.Int).SetBytes(block.IP)
		for i := big.NewInt(0); i.Cmp(count) < 0; i.Add(i, big.NewInt(1)) {
			addr := new(big.Int).Add(start, new(big.Int).Mul(i, step))
			cand := &net.IPNet{IP: bigToIP(addr, len(block.IP)), Mask: net.CIDRMask(size, bits)}
			if a.overlapsUsedLocked(pool, cand) || overlapsNets(cand, avoid) {
				continue
			}
			return cand.String(), true
		}
	}
	return "", false
}

func overlapsNets(n *net.IPNet, others []*net.IPNet) bool {
	for _, o := range others {
		if cidrsOverlap(n, o) {
			return true
		}
	}
	return false
}

func overlapsAny(blocks []AddressBlock, avoid []*net.IPNet) bool {
	for _, b := range blocks {
		if _, n, err := net.ParseCIDR(b.CIDR); err == nil && overlapsNets(n, avoid) {
			return true
		}
	}
	return false
}

func insideAny(n *net.IPNet, blocks []string) bool {
	nOnes, _ := n.Mask.Size()
	for _, b := range blocks {
		_, block, err := net.ParseCIDR(b)
		if err != nil {
			continue
		}
		bOnes, _ := block.Mask.Size()
		if bOnes <= nOnes && block.Contains(n.IP) {
			return true
		}
	}
	return false
}

func bigToIP(v *big.Int, length int) net.IP {
	b := v.Bytes()
	ip := make(net.IP, length)
	copy(ip[length-len(b):], b)
	return ip
}

// gatewayFor derives a gateway: the first usable address (network+1) or
// the last (IPv4: broadcast-1; IPv6: the last address).
func gatewayFor(cidr string, placement GatewayPlacement) string {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ones, bits := n.Mask.Size()
	base := new(big.Int).SetBytes(n.IP)
	size := new(big.Int).Lsh(big.NewInt(1), uint(bits-ones))
	var v *big.Int
	if placement == GatewayLast {
		v = new(big.Int).Add(base, size)
		v.Sub(v, big.NewInt(1)) // last address
		if bits == 32 {
			v.Sub(v, big.NewInt(1)) // skip the IPv4 broadcast address
		}
	} else {
		v = new(big.Int).Add(base, big.NewInt(1))
	}
	return bigToIP(v, len(n.IP)).String()
}

func familyOf(cidr string) AddressFamily {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		ip = net.ParseIP(cidr)
	}
	if ip != nil && ip.To4() == nil {
		return FamilyIPv6
	}
	return FamilyIPv4
}

// ---------------------------------------------------------------------------
// Validation: shape only, never data-plane meaning (docs/specs/network.md).

func validatePoolSpec(s AllocationPoolSpec) error {
	kinds := 0
	if s.Entries != nil {
		kinds++
	}
	if s.Integer != nil {
		kinds++
	}
	if s.Cidr != nil {
		kinds++
	}
	if kinds != 1 {
		return fmt.Errorf("%w: exactly one of entries/integer/cidr must be set", ErrValidation)
	}
	switch s.Kind() {
	case PoolKindInteger:
		if len(s.Integer) == 0 {
			return fmt.Errorf("%w: integer pool needs at least one range", ErrValidation)
		}
		for _, r := range s.Integer {
			if r.Lo > r.Hi {
				return fmt.Errorf("%w: integer range %d-%d: lo > hi", ErrValidation, r.Lo, r.Hi)
			}
		}
	case PoolKindEntries:
		seen := map[string]bool{}
		for _, e := range s.Entries {
			if e.Key == "" || seen[e.Key] {
				return fmt.Errorf("%w: entry keys must be non-empty and unique (%q)", ErrValidation, e.Key)
			}
			seen[e.Key] = true
			families := map[AddressFamily]bool{}
			for _, b := range e.Addresses {
				if err := validateAddressBlock(b); err != nil {
					return err
				}
				f := familyOf(b.CIDR)
				if families[f] {
					return fmt.Errorf("%w: entry %q has two %s addresses", ErrValidation, e.Key, f)
				}
				families[f] = true
			}
		}
	case PoolKindCidr:
		c := *s.Cidr
		switch c.Mode {
		case CidrModeUserAny:
		case CidrModeUserWithinBlocks, CidrModeCarve:
			if len(c.Blocks) == 0 {
				return fmt.Errorf("%w: CIDR pool mode %s needs blocks", ErrValidation, c.Mode)
			}
			for _, b := range c.Blocks {
				ip, n, err := net.ParseCIDR(b)
				if err != nil || !ip.Equal(n.IP) {
					return fmt.Errorf("%w: block %q is not a network address CIDR", ErrValidation, b)
				}
				if f := familyOf(b); f != familyOrDefault(c.Family) {
					return fmt.Errorf("%w: block %q is not %s", ErrValidation, b, familyOrDefault(c.Family))
				}
			}
			if c.Mode == CidrModeCarve {
				_, bits := net.CIDRMask(0, map[AddressFamily]int{FamilyIPv4: 32, FamilyIPv6: 128}[familyOrDefault(c.Family)]).Size()
				if c.PrefixLength < 1 || int(c.PrefixLength) > bits {
					return fmt.Errorf("%w: CARVE needs a prefix_length within 1-%d", ErrValidation, bits)
				}
			}
		default:
			return fmt.Errorf("%w: CIDR pool needs a mode (USER_ANY/USER_WITHIN_BLOCKS/CARVE)", ErrValidation)
		}
	}
	return nil
}

func familyOrDefault(f AddressFamily) AddressFamily {
	if f == "" {
		return FamilyIPv4
	}
	return f
}

func validateAddressBlock(b AddressBlock) error {
	ip, n, err := net.ParseCIDR(b.CIDR)
	if err != nil || !ip.Equal(n.IP) {
		return fmt.Errorf("%w: address %q is not a network address CIDR", ErrValidation, b.CIDR)
	}
	if b.GatewayIP != "" {
		gw := net.ParseIP(b.GatewayIP)
		if gw == nil || !n.Contains(gw) {
			return fmt.Errorf("%w: gateway %q is not inside %s", ErrValidation, b.GatewayIP, b.CIDR)
		}
	}
	return nil
}

// cidrSources reports, per address family, which pool (if any) supplies a
// zone's CIDR, and rejects a second source for the same family or a value
// name drawn from two pools.
func cidrSources(pools map[string]AllocationPool, refs []PoolRef) (map[AddressFamily]AllocationPool, error) {
	sources := map[AddressFamily]AllocationPool{}
	names := map[string]string{}
	claim := func(name, pool string) error {
		if other, ok := names[name]; ok && other != pool {
			return fmt.Errorf("%w: value %q would come from two pools (%s, %s)", ErrValidation, name, other, pool)
		}
		names[name] = pool
		return nil
	}
	addSource := func(f AddressFamily, p AllocationPool) error {
		if other, ok := sources[f]; ok {
			return fmt.Errorf("%w: two %s CIDR sources (%s, %s)", ErrValidation, f, other.Meta.Name, p.Meta.Name)
		}
		sources[f] = p
		return nil
	}
	for _, ref := range refs {
		p, ok := pools[ref.PoolID]
		if !ok {
			return nil, fmt.Errorf("%w: pool %q does not exist", ErrValidation, ref.PoolID)
		}
		switch p.Spec.Kind() {
		case PoolKindInteger:
			if ref.Name == "" {
				return nil, fmt.Errorf("%w: a reference to integer pool %q needs a name", ErrValidation, p.Meta.Name)
			}
			if err := claim(ref.Name, p.Meta.ID); err != nil {
				return nil, err
			}
		case PoolKindEntries:
			if ref.Name != "" {
				return nil, fmt.Errorf("%w: a reference to entries pool %q must not have a name", ErrValidation, p.Meta.Name)
			}
			families := map[AddressFamily]bool{}
			for _, e := range p.Spec.Entries {
				for name := range e.Values {
					if err := claim(name, p.Meta.ID); err != nil {
						return nil, err
					}
				}
				for _, b := range e.Addresses {
					families[familyOf(b.CIDR)] = true
				}
			}
			for f := range families {
				if err := addSource(f, p); err != nil {
					return nil, err
				}
			}
		case PoolKindCidr:
			if ref.Name != "" {
				return nil, fmt.Errorf("%w: a reference to CIDR pool %q must not have a name", ErrValidation, p.Meta.Name)
			}
			if err := addSource(familyOrDefault(p.Spec.Cidr.Family), p); err != nil {
				return nil, err
			}
		}
	}
	return sources, nil
}

// validateClassSpec checks a NetworkClass's structure against the pools
// it references.
func validateClassSpec(c NetworkClassSpec, pools map[string]AllocationPool) error {
	if _, err := cidrSources(pools, c.Network); err != nil {
		return err
	}
	for _, ref := range c.Network {
		if pools[ref.PoolID].Spec.Kind() == PoolKindCidr {
			return fmt.Errorf("%w: a Network-level reference can't be a CIDR pool (addresses belong to Subnets)", ErrValidation)
		}
	}
	zones := slices.Collect(maps.Keys(c.Subnet))
	if len(zones) == 0 {
		return fmt.Errorf("%w: subnet pool references are required (\"*\" or per zone)", ErrValidation)
	}
	for _, z := range zones {
		if _, err := cidrSources(pools, c.SubnetRefs(z)); err != nil {
			return fmt.Errorf("zone %q: %w", z, err)
		}
	}
	switch c.GatewayPlacement {
	case "", GatewayFirst, GatewayLast:
	default:
		return fmt.Errorf("%w: gateway_placement %q", ErrValidation, c.GatewayPlacement)
	}
	if c.MTU < 0 || (c.MTU != 0 && c.MTU < 576) {
		return fmt.Errorf("%w: mtu %d is too small", ErrValidation, c.MTU)
	}
	for z, servers := range c.DefaultDNSServers {
		for _, srv := range servers {
			if net.ParseIP(srv) == nil {
				return fmt.Errorf("%w: default_dns_servers[%q]: %q is not an IP", ErrValidation, z, srv)
			}
		}
	}
	return nil
}
