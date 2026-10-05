package network

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

func mustPool(t *testing.T, ctx context.Context, svc *Service, name string, spec AllocationPoolSpec) string {
	t.Helper()
	p, err := svc.CreateAllocationPool(ctx, name, spec, resource.Metadata{})
	if err != nil {
		t.Fatalf("CreateAllocationPool %s: %v", name, err)
	}
	return p.Meta.ID
}

func mustClass(t *testing.T, ctx context.Context, svc *Service, name string, spec NetworkClassSpec) string {
	t.Helper()
	c, err := svc.CreateNetworkClass(ctx, name, spec, resource.Metadata{})
	if err != nil {
		t.Fatalf("CreateNetworkClass %s: %v", name, err)
	}
	return c.Meta.ID
}

// carveNetwork returns tenantID's Ready Network of a class that allocates
// everything itself: a route target per Network, and per Subnet a vlan_id
// plus a /24 carved out of 10.10.0.0/16.
func carveNetwork(t *testing.T, ctx context.Context, svc *Service, tenantID string) string {
	t.Helper()
	rt := mustPool(t, ctx, svc, "rt", AllocationPoolSpec{Integer: []IntRange{{65000, 65999}}})
	vlan := mustPool(t, ctx, svc, "vlan", AllocationPoolSpec{Integer: []IntRange{{100, 199}}})
	carve := mustPool(t, ctx, svc, "carve", AllocationPoolSpec{Cidr: &CidrPoolSpec{Mode: CidrModeCarve, Blocks: []string{"10.10.0.0/16"}, PrefixLength: 24}})
	class := mustClass(t, ctx, svc, "carve", NetworkClassSpec{
		Network:    []PoolRef{{PoolID: rt, Name: "route_target"}},
		Subnet:     map[string][]PoolRef{"*": {{PoolID: carve}, {PoolID: vlan, Name: "vlan_id"}}},
		Visibility: VisibilityPublic,
	})
	return mustReadyNetwork(t, ctx, svc, tenantID, "carve-net", NetworkSpec{NetworkClass: class})
}

func TestPool_ValidatesShape(t *testing.T) {
	svc, ctx := newTestService(t)
	for name, spec := range map[string]AllocationPoolSpec{
		"no kind":            {},
		"two kinds":          {Integer: []IntRange{{1, 2}}, Cidr: &CidrPoolSpec{Mode: CidrModeUserAny}},
		"empty integer":      {Integer: []IntRange{}},
		"inverted range":     {Integer: []IntRange{{5, 1}}},
		"duplicate entry":    {Entries: []PoolEntry{{Key: "a"}, {Key: "a"}}},
		"entry bad cidr":     {Entries: []PoolEntry{{Key: "a", Addresses: []AddressBlock{{CIDR: "10.0.0.1/24"}}}}},
		"entry two v4":       {Entries: []PoolEntry{{Key: "a", Addresses: []AddressBlock{{CIDR: "10.0.0.0/24"}, {CIDR: "10.0.1.0/24"}}}}},
		"carve no blocks":    {Cidr: &CidrPoolSpec{Mode: CidrModeCarve, PrefixLength: 24}},
		"carve bad prefix":   {Cidr: &CidrPoolSpec{Mode: CidrModeCarve, Blocks: []string{"10.0.0.0/16"}, PrefixLength: 40}},
		"block wrong family": {Cidr: &CidrPoolSpec{Mode: CidrModeCarve, Blocks: []string{"fd00::/48"}, PrefixLength: 24}},
		"cidr no mode":       {Cidr: &CidrPoolSpec{}},
	} {
		if _, err := svc.CreateAllocationPool(ctx, "p-"+name, spec, resource.Metadata{}); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: got %v, want ErrValidation", name, err)
		}
	}
}

func TestClass_ValidatesStructure(t *testing.T) {
	svc, ctx := newTestService(t)
	userA := mustPool(t, ctx, svc, "user-a", AllocationPoolSpec{Cidr: &CidrPoolSpec{Mode: CidrModeUserAny}})
	userB := mustPool(t, ctx, svc, "user-b", AllocationPoolSpec{Cidr: &CidrPoolSpec{Mode: CidrModeUserAny}})
	ints := mustPool(t, ctx, svc, "ints", AllocationPoolSpec{Integer: []IntRange{{1, 10}}})
	ints2 := mustPool(t, ctx, svc, "ints2", AllocationPoolSpec{Integer: []IntRange{{1, 10}}})
	for name, spec := range map[string]NetworkClassSpec{
		"no subnet refs":       {},
		"two v4 sources":       {Subnet: map[string][]PoolRef{"*": {{PoolID: userA}, {PoolID: userB}}}},
		"two v4 via override":  {Subnet: map[string][]PoolRef{"*": {{PoolID: userA}}, "az-a": {{PoolID: userB}}}},
		"integer without name": {Subnet: map[string][]PoolRef{"*": {{PoolID: userA}, {PoolID: ints}}}},
		"name from two pools":  {Subnet: map[string][]PoolRef{"*": {{PoolID: userA}, {PoolID: ints, Name: "x"}}, "az-a": {{PoolID: ints2, Name: "y"}, {PoolID: ints, Name: "y"}}}},
		"cidr named":           {Subnet: map[string][]PoolRef{"*": {{PoolID: userA, Name: "x"}}}},
		"network-level cidr":   {Network: []PoolRef{{PoolID: userA}}, Subnet: map[string][]PoolRef{"*": {{PoolID: userB}}}},
		"unknown pool":         {Subnet: map[string][]PoolRef{"*": {{PoolID: "allocpool-nope"}}}},
		"bad dns":              {Subnet: map[string][]PoolRef{"*": {{PoolID: userA}}}, DefaultDNSServers: map[string][]string{"*": {"nope"}}},
		"tiny mtu":             {Subnet: map[string][]PoolRef{"*": {{PoolID: userA}}}, MTU: 100},
	} {
		if _, err := svc.CreateNetworkClass(ctx, "c-"+name, spec, resource.Metadata{}); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: got %v, want ErrValidation", name, err)
		}
	}
}

func TestSubnet_AllocatesFromClassPools(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := carveNetwork(t, ctx, svc, "tenant-a")
	n, _ := svc.GetNetwork(ctx, "tenant-a", netID)
	if n.Status.Values["route_target"] != 65000 {
		t.Fatalf("network values = %v, want route_target=65000", n.Status.Values)
	}

	a := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "a", SubnetSpec{NetworkID: netID, Zone: "zone-a"})
	b := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "b", SubnetSpec{NetworkID: netID, Zone: "zone-b"})
	for _, tc := range []struct {
		sn       *Subnet
		cidr, gw string
		vlan     int64
	}{{a, "10.10.0.0/24", "10.10.0.1", 100}, {b, "10.10.1.0/24", "10.10.1.1", 101}} {
		cidr, gw := tc.sn.Status.IPv4()
		if tc.sn.Status.Phase != SubnetPhaseReady || cidr != tc.cidr || gw != tc.gw || tc.sn.Status.Values["vlan_id"] != tc.vlan {
			t.Fatalf("subnet %s = %+v, want Ready %s gw %s vlan_id %d", tc.sn.Meta.Name, tc.sn.Status, tc.cidr, tc.gw, tc.vlan)
		}
	}
	// A carved CIDR is never requested by the user.
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "c", SubnetSpec{NetworkID: netID, Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "10.99.0.0/24"}}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("requested CIDR on a carving class: got %v, want ErrValidation", err)
	}
}

func TestSubnet_GatewayPlacementLast(t *testing.T) {
	svc, ctx := newTestService(t)
	carve := mustPool(t, ctx, svc, "carve", AllocationPoolSpec{Cidr: &CidrPoolSpec{Mode: CidrModeCarve, Blocks: []string{"10.20.0.0/16"}, PrefixLength: 24}})
	class := mustClass(t, ctx, svc, "last", NetworkClassSpec{Subnet: map[string][]PoolRef{"*": {{PoolID: carve}}}, Visibility: VisibilityPublic, GatewayPlacement: GatewayLast})
	netID := mustReadyNetwork(t, ctx, svc, "tenant-a", "n", NetworkSpec{NetworkClass: class})
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z"})
	if _, gw := sn.Status.IPv4(); gw != "10.20.0.254" {
		t.Fatalf("gateway = %s, want 10.20.0.254", gw)
	}
}

func TestSubnet_EntriesPoolHandsOutWholeTuplesAndReusesAfterDelete(t *testing.T) {
	svc, ctx := newTestService(t)
	entries := mustPool(t, ctx, svc, "az-a-vlans", AllocationPoolSpec{
		Entries: []PoolEntry{
			{Key: "vlan-300", Values: map[string]int64{"vlan_id": 300}, Addresses: []AddressBlock{{CIDR: "192.168.30.0/24", GatewayIP: "192.168.30.254"}}, Attributes: map[string]string{"fabric": "east"}},
		},
		Attributes: map[string]string{"kind": "vlan"},
	})
	vni := mustPool(t, ctx, svc, "vni", AllocationPoolSpec{Integer: []IntRange{{5000, 5999}}})
	class := mustClass(t, ctx, svc, "vlan-std", NetworkClassSpec{
		// "*" gives every zone a VNI; az-a adds its static VLAN tuples on top.
		Subnet:     map[string][]PoolRef{"*": {{PoolID: vni, Name: "vni"}}, "az-a": {{PoolID: entries}}},
		Visibility: VisibilityPublic,
	})
	netID := mustReadyNetwork(t, ctx, svc, "tenant-a", "n", NetworkSpec{NetworkClass: class})

	// Zone without any address source can't host a Subnet.
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "nope", SubnetSpec{NetworkID: netID, Zone: "az-b"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("zone with no IPv4 source: got %v, want ErrValidation", err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go svc.watchPendingSubnets(watchCtx)
	first := createAndWaitReady(t, ctx, svc, "tenant-a", "s1", SubnetSpec{NetworkID: netID, Zone: "az-a"})
	cidr, gw := first.Status.IPv4()
	if cidr != "192.168.30.0/24" || gw != "192.168.30.254" || first.Status.Values["vlan_id"] != 300 || first.Status.Values["vni"] != 5000 ||
		first.Status.Attributes["fabric"] != "east" || first.Status.Attributes["kind"] != "vlan" {
		t.Fatalf("first subnet = %+v", first.Status)
	}

	second, err := svc.CreateSubnet(ctx, "tenant-a", "s2", SubnetSpec{NetworkID: netID, Zone: "az-a"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	second, _ = svc.GetSubnet(ctx, "tenant-a", second.Meta.ID)
	if second.Status.Phase != SubnetPhasePending || !hasTrueCondition(second.Status.Conditions, "AllocationPending") {
		t.Fatalf("second subnet with the only entry taken = %+v, want Pending with AllocationPending", second.Status)
	}
	// The VNI drawn for the failed attempt must have been returned.
	if got := svc.alloc.ints[vni]; len(got) != 1 {
		t.Fatalf("vni pool holds %v after a failed all-or-nothing attempt, want just the first subnet's", got)
	}

	if err := svc.DeleteSubnet(ctx, "tenant-a", first.Meta.ID); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		svc.retryPendingSubnets(ctx)
		if second, _ = svc.GetSubnet(ctx, "tenant-a", second.Meta.ID); second.Status.Phase == SubnetPhaseReady {
			break
		}
	}
	if second.Status.Values["vlan_id"] != 300 {
		t.Fatalf("after the first subnet was deleted, second = %+v, want the freed vlan-300 tuple", second.Status)
	}
}

func hasTrueCondition(conds []resource.Condition, typ string) bool {
	return slices.ContainsFunc(conds, func(c resource.Condition) bool { return c.Type == typ && c.Status == resource.ConditionTrue })
}

func TestPoolAndClass_InUseProtection(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := carveNetwork(t, ctx, svc, "tenant-a")
	mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "a", SubnetSpec{NetworkID: netID, Zone: "z"})
	pools, _ := svc.ListAllocationPools(ctx)
	byName := map[string]AllocationPool{}
	for _, p := range pools {
		byName[p.Meta.Name] = p
	}
	vlan := byName["vlan"]
	if vlan.Status.Allocated != 1 {
		t.Fatalf("vlan pool allocated = %d, want 1", vlan.Status.Allocated)
	}
	vlan.Spec.Integer = []IntRange{{150, 199}} // drops the allocated 100
	if _, err := svc.UpdateAllocationPool(ctx, &vlan); !errors.Is(err, ErrInUse) {
		t.Fatalf("shrinking past an allocation: got %v, want ErrInUse", err)
	}
	vlan.Spec.Integer = []IntRange{{100, 299}} // widening is fine
	if _, err := svc.UpdateAllocationPool(ctx, &vlan); err != nil {
		t.Fatalf("widening: %v", err)
	}
	if err := svc.DeleteAllocationPool(ctx, vlan.Meta.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting a pool with allocations: got %v, want ErrInUse", err)
	}
	n, _ := svc.GetNetwork(ctx, "tenant-a", netID)
	if err := svc.DeleteNetworkClass(ctx, n.Spec.NetworkClass); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting a class in use: got %v, want ErrInUse", err)
	}
	if err := svc.DeleteNetwork(ctx, "tenant-a", netID); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting a Network with Subnets: got %v, want ErrInUse", err)
	}
}

func TestNetwork_VisibilityAndSharing(t *testing.T) {
	svc, ctx := newTestService(t)
	user := mustPool(t, ctx, svc, "user", AllocationPoolSpec{Cidr: &CidrPoolSpec{Mode: CidrModeUserAny}})
	private := mustClass(t, ctx, svc, "private", NetworkClassSpec{Subnet: map[string][]PoolRef{"*": {{PoolID: user}}}, SharedWithTenantIDs: []string{"tenant-a"}})
	publicOK := mustClass(t, ctx, svc, "public-ok", NetworkClassSpec{Subnet: map[string][]PoolRef{"*": {{PoolID: user}}}, Visibility: VisibilityPublic, AllowPublicNetworks: true})

	if _, err := svc.CreateNetwork(ctx, "tenant-b", "n", NetworkSpec{NetworkClass: private}, resource.Metadata{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("class not shared with tenant-b: got %v, want ErrValidation", err)
	}
	if _, err := svc.CreateNetwork(ctx, "tenant-a", "pub", NetworkSpec{NetworkClass: private, Visibility: VisibilityPublic}, resource.Metadata{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("PUBLIC on a class without allow_public_networks: got %v, want ErrValidation", err)
	}
	mustReadyNetwork(t, ctx, svc, "tenant-a", "pub-ok", NetworkSpec{NetworkClass: publicOK, Visibility: VisibilityPublic})

	shared := mustReadyNetwork(t, ctx, svc, "tenant-a", "shared", NetworkSpec{NetworkClass: private, SharedWithTenantIDs: []string{"tenant-b"}})
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: shared, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.5.0.0/24"}}})
	nic := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-b", "nic", NetworkInterfaceSpec{VMID: "vm", NetworkID: shared, Zone: "z"}, sn)
	if nic.Status.Phase != NetworkInterfacePhaseReady || nic.Status.SubnetID != sn.Meta.ID {
		t.Fatalf("shared tenant's NIC = %+v", nic.Status)
	}
	if _, err := svc.GetSubnet(ctx, "tenant-b", sn.Meta.ID); err != nil {
		t.Fatalf("shared tenant can't read the Subnet its NIC is on: %v", err)
	}
	if _, err := svc.CreateNetworkInterface(ctx, "tenant-c", "nic", NetworkInterfaceSpec{VMID: "vm", NetworkID: shared, Zone: "z"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("tenant-c not shared: got %v, want ErrValidation", err)
	}
	if _, err := svc.GetNetwork(ctx, "tenant-c", shared); !errors.Is(err, ErrNetworkNotFound) {
		t.Fatalf("tenant-c reading the Network: got %v, want NotFound", err)
	}
}

func TestNetworkInterface_NetworkOnlyPicksASubnetWithRoom(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := testNetwork(t, ctx, svc, "tenant-a")
	// Two /30s with .1 gateways: one address each.
	s1 := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s1", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.0/30"}}})
	time.Sleep(5 * time.Millisecond) // order by creation
	s2 := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s2", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.4/30"}}})
	mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "other-zone", SubnetSpec{NetworkID: netID, Zone: "y", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.8/30"}}})

	var got []string
	for _, name := range []string{"n1", "n2", "n3"} {
		n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", name, NetworkInterfaceSpec{VMID: "vm", NetworkID: netID, Zone: "z"}, nil)
		got = append(got, n.Status.SubnetID)
		if name == "n3" && (n.Status.Phase != NetworkInterfacePhasePending || !hasTrueCondition(n.Status.Conditions, "NoFreeAddress")) {
			t.Fatalf("third NIC = %+v, want Pending with NoFreeAddress", n.Status)
		}
	}
	if got[0] != s1.Meta.ID || got[1] != s2.Meta.ID || got[2] != "" {
		t.Fatalf("NIC subnets = %v, want [%s %s \"\"]", got, s1.Meta.ID, s2.Meta.ID)
	}
	// Adding a Subnet unblocks it on the next sweep.
	s3 := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s3", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.12/30"}}})
	svc.retryPendingNetworkInterfaces(ctx)
	ifaces, _ := svc.ListNetworkInterfaces(ctx, "tenant-a")
	for _, n := range ifaces {
		if n.Meta.Name == "n3" && n.Status.SubnetID != s3.Meta.ID {
			t.Fatalf("n3 after adding s3 = %+v", n.Status)
		}
	}
}
