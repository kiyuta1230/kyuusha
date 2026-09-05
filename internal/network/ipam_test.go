package network

import "testing"

func TestVLANPool_AllocateIsExclusivePerZone(t *testing.T) {
	p := newVLANPool()

	id1, ok := p.allocate("zone-a")
	if !ok || id1 != minVLANID {
		t.Fatalf("expected first allocation to be %d, got %d (ok=%v)", minVLANID, id1, ok)
	}
	id2, ok := p.allocate("zone-a")
	if !ok || id2 == id1 {
		t.Fatalf("expected a distinct id from zone-a's pool, got %d again", id2)
	}
	id3, ok := p.allocate("zone-b")
	if !ok || id3 != minVLANID {
		t.Fatalf("expected zone-b's independent pool to start at %d too, got %d", minVLANID, id3)
	}
}

func TestVLANPool_ReleaseMakesIDReusable(t *testing.T) {
	p := newVLANPool()

	id, _ := p.allocate("zone-a")
	p.release("zone-a", id)
	again, ok := p.allocate("zone-a")
	if !ok || again != id {
		t.Fatalf("expected the released id %d to be reused, got %d", id, again)
	}
}

func TestVLANPool_ExhaustsAtMax(t *testing.T) {
	p := newVLANPool()
	for i := minVLANID; i <= maxVLANID; i++ {
		if _, ok := p.allocate("zone-a"); !ok {
			t.Fatalf("pool unexpectedly exhausted after %d allocations", i-minVLANID)
		}
	}
	if _, ok := p.allocate("zone-a"); ok {
		t.Fatal("expected the pool to be exhausted after allocating its full range")
	}
}

func TestIPPool_AllocateExcludesNetworkBroadcastAndGateway(t *testing.T) {
	p := newIPPool()

	// 10.0.1.0/30: network=.0, broadcast=.3, usable={.1,.2}. With .1 as
	// gateway, only .2 should ever be handed out.
	ip1, ok := p.allocate("subnet-a", "10.0.1.0/30", "10.0.1.1")
	if !ok || ip1 != "10.0.1.2" {
		t.Fatalf("expected 10.0.1.2 (only non-gateway usable address), got %q (ok=%v)", ip1, ok)
	}
	if _, ok := p.allocate("subnet-a", "10.0.1.0/30", "10.0.1.1"); ok {
		t.Fatal("expected the pool to report exhausted once the only usable address is gone")
	}
}

func TestIPPool_TooSmallCIDRAlwaysExhausted(t *testing.T) {
	p := newIPPool()
	if _, ok := p.allocate("subnet-a", "10.0.1.0/31", ""); ok {
		t.Fatal("expected /31 to have no usable host range in this model")
	}
	if _, ok := p.allocate("subnet-a", "10.0.1.1/32", ""); ok {
		t.Fatal("expected /32 to have no usable host range in this model")
	}
}

func TestIPPool_ReleaseMakesAddressReusable(t *testing.T) {
	p := newIPPool()

	// allocate always scans from the lowest usable address, so releasing
	// the first one handed out makes it the very next result again.
	ip1, _ := p.allocate("subnet-a", "10.0.1.0/29", "")
	ip2, _ := p.allocate("subnet-a", "10.0.1.0/29", "")
	if ip1 == ip2 {
		t.Fatalf("expected two distinct addresses, got %s twice", ip1)
	}
	p.release("subnet-a", ip1)
	again, ok := p.allocate("subnet-a", "10.0.1.0/29", "")
	if !ok || again != ip1 {
		t.Fatalf("expected the released address %s to be reused, got %q (ok=%v)", ip1, again, ok)
	}
}
