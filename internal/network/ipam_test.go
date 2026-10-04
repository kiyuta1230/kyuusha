package network

import "testing"

func TestIPPool_AllocateExcludesNetworkBroadcastAndGateway(t *testing.T) {
	p := newIPPool()

	// 10.0.1.0/30: network=.0, broadcast=.3, usable={.1,.2}. With .1 as
	// gateway, only .2 should ever be handed out.
	ip1, ok := p.allocate("subnet-a", "10.0.1.0/30", "10.0.1.1", nil)
	if !ok || ip1 != "10.0.1.2" {
		t.Fatalf("expected 10.0.1.2 (only non-gateway usable address), got %q (ok=%v)", ip1, ok)
	}
	if _, ok := p.allocate("subnet-a", "10.0.1.0/30", "10.0.1.1", nil); ok {
		t.Fatal("expected the pool to report exhausted once the only usable address is gone")
	}
}

func TestIPPool_TooSmallCIDRAlwaysExhausted(t *testing.T) {
	p := newIPPool()
	if _, ok := p.allocate("subnet-a", "10.0.1.0/31", "", nil); ok {
		t.Fatal("expected /31 to have no usable host range in this model")
	}
	if _, ok := p.allocate("subnet-a", "10.0.1.1/32", "", nil); ok {
		t.Fatal("expected /32 to have no usable host range in this model")
	}
}

func TestIPPool_AllocatableRangesRestrictThePool(t *testing.T) {
	p := newIPPool()

	// 10.0.1.0/24 has 254 usable addresses, but only .10-.11 are allowed.
	ranges := []string{"10.0.1.10-10.0.1.11"}
	ip1, ok := p.allocate("subnet-a", "10.0.1.0/24", "", ranges)
	if !ok || ip1 != "10.0.1.10" {
		t.Fatalf("expected 10.0.1.10, got %q (ok=%v)", ip1, ok)
	}
	ip2, ok := p.allocate("subnet-a", "10.0.1.0/24", "", ranges)
	if !ok || ip2 != "10.0.1.11" {
		t.Fatalf("expected 10.0.1.11, got %q (ok=%v)", ip2, ok)
	}
	if _, ok := p.allocate("subnet-a", "10.0.1.0/24", "", ranges); ok {
		t.Fatal("expected the pool to be exhausted once both addresses in the range are gone, even though the rest of the /24 is free")
	}
}

func TestIPPool_AllocatableRangesAreClampedToTheUsableHostRange(t *testing.T) {
	p := newIPPool()

	// A range that (mistakenly, or just generously) includes the network
	// and broadcast addresses of 10.0.1.0/30 must still never hand them out.
	ranges := []string{"10.0.1.0-10.0.1.3"}
	ip, ok := p.allocate("subnet-a", "10.0.1.0/30", "", ranges)
	if !ok || (ip != "10.0.1.1" && ip != "10.0.1.2") {
		t.Fatalf("expected a usable host address, got %q (ok=%v)", ip, ok)
	}
}

func TestValidateAllocatableIPRanges(t *testing.T) {
	if err := validateAllocatableIPRanges("10.0.1.0/24", []string{"10.0.1.10-10.0.1.20"}); err != nil {
		t.Fatalf("expected a range inside the cidr to validate, got %v", err)
	}
	if err := validateAllocatableIPRanges("10.0.1.0/24", []string{"10.0.2.10-10.0.2.20"}); err == nil {
		t.Fatal("expected a range outside the cidr to be rejected")
	}
	if err := validateAllocatableIPRanges("10.0.1.0/24", []string{"not-an-ip-range"}); err == nil {
		t.Fatal("expected an unparseable range to be rejected")
	}
	if err := validateAllocatableIPRanges("10.0.1.0/24", []string{"10.0.1.20-10.0.1.10"}); err == nil {
		t.Fatal("expected a range with start after end to be rejected")
	}
}

func TestIPPool_ReleaseMakesAddressReusable(t *testing.T) {
	p := newIPPool()

	// allocate always scans from the lowest usable address, so releasing
	// the first one handed out makes it the very next result again.
	ip1, _ := p.allocate("subnet-a", "10.0.1.0/29", "", nil)
	ip2, _ := p.allocate("subnet-a", "10.0.1.0/29", "", nil)
	if ip1 == ip2 {
		t.Fatalf("expected two distinct addresses, got %s twice", ip1)
	}
	p.release("subnet-a", ip1)
	again, ok := p.allocate("subnet-a", "10.0.1.0/29", "", nil)
	if !ok || again != ip1 {
		t.Fatalf("expected the released address %s to be reused, got %q (ok=%v)", ip1, again, ok)
	}
}
