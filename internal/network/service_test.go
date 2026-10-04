package network

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func newTestService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, ctx
}

func TestService_CreateSubnetValidatesSpec(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := testNetwork(t, ctx, svc, "tenant-a")

	for name, spec := range map[string]SubnetSpec{
		"missing network":    {Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.1.0/24"}}},
		"unknown network":    {NetworkID: "network-nope", Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.1.0/24"}}},
		"missing zone":       {NetworkID: netID, RequestedAddresses: []SubnetAddress{{CIDR: "10.0.1.0/24"}}},
		"missing cidr":       {NetworkID: netID, Zone: "zone-a"}, // the class expects a user-specified CIDR
		"bad cidr":           {NetworkID: netID, Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "not-a-cidr"}}},
		"gateway outside":    {NetworkID: netID, Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.1.0/24", GatewayIP: "10.9.9.9"}}},
		"bad dns server":     {NetworkID: netID, Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.1.0/24"}}, DNSServers: []string{"nope"}},
		"range outside cidr": {NetworkID: netID, Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.1.0/24"}}, AllocatableIPRanges: []string{"10.0.9.1-10.0.9.9"}},
	} {
		if _, err := svc.CreateSubnet(ctx, "tenant-a", "x-"+name, spec); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: got %v, want ErrValidation", name, err)
		}
	}
	// Another tenant's Network: only its owner adds Subnets.
	if _, err := svc.CreateSubnet(ctx, "tenant-b", "foreign", SubnetSpec{NetworkID: netID, Zone: "zone-a", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.1.0/24"}}}); !errors.Is(err, ErrValidation) {
		t.Errorf("Subnet on another tenant's Network: got %v, want ErrValidation", err)
	}
}

func TestService_CreateSubnetRejectsOverlapWithinNetwork(t *testing.T) {
	svc, ctx := newTestService(t)
	mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", ""))
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "sn2", userSubnet(t, ctx, svc, "tenant-a", "zone-b", "10.0.1.128/25", "")); !errors.Is(err, ErrValidation) {
		t.Fatalf("overlapping CIDR in the same Network (even another zone): got %v, want ErrValidation", err)
	}
	// A different Network is a different routing domain: overlap is fine.
	if _, err := svc.CreateSubnet(ctx, "tenant-b", "sn3", userSubnet(t, ctx, svc, "tenant-b", "zone-a", "10.0.1.0/24", "")); err != nil {
		t.Fatalf("same CIDR in another Network: %v", err)
	}
}

func TestService_CreateNetworkInterfaceRespectsAllocatableIPRanges(t *testing.T) {
	svc, ctx := newTestService(t)
	spec := userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", "10.0.1.1")
	spec.AllocatableIPRanges = []string{"10.0.1.50-10.0.1.51"}
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", spec)
	for i, want := range []string{"10.0.1.50", "10.0.1.51"} {
		n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic"+want, NetworkInterfaceSpec{VMID: "vm", SubnetID: sn.Meta.ID}, sn)
		if n.Status.IPAddress != want {
			t.Fatalf("NIC %d got %s, want %s", i, n.Status.IPAddress, want)
		}
	}
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic-overflow", NetworkInterfaceSpec{VMID: "vm", SubnetID: sn.Meta.ID}, sn)
	if n.Status.Phase != NetworkInterfacePhasePending {
		t.Fatalf("third NIC: phase %s, want Pending (ranges exhausted)", n.Status.Phase)
	}
}

func TestService_CreateSubnetIsIdempotentByName(t *testing.T) {
	svc, ctx := newTestService(t)
	spec := userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", "")
	a, err := svc.CreateSubnet(ctx, "tenant-a", "sn", spec)
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	b, err := svc.CreateSubnet(ctx, "tenant-a", "sn", spec)
	if err != nil {
		t.Fatalf("CreateSubnet again: %v", err)
	}
	if a.Meta.ID != b.Meta.ID {
		t.Fatalf("second Create made a new Subnet (%s != %s)", b.Meta.ID, a.Meta.ID)
	}
}

func TestService_CreateNeverAllocatesSynchronously(t *testing.T) {
	svc, ctx := newTestService(t)
	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", ""))
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if sn.Status.Phase != SubnetPhasePending || len(sn.Status.Addresses) != 0 {
		t.Fatalf("CreateSubnet allocated synchronously: %+v", sn.Status)
	}
	svc.tryAllocateSubnet(ctx, sn)

	n, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
	if n.Status.Phase != NetworkInterfacePhasePending || n.Status.IPAddress != "" || n.Status.MACAddress != "" {
		t.Fatalf("CreateNetworkInterface allocated synchronously: %+v", n.Status)
	}
}

// TestService_NewServiceRebuildsPoolsFromExistingResources: every pool is
// purely in-memory, so a restarted process (a second NewService against the
// same etcd) must rebuild them before handing anything out -- pool values,
// IPs and MACs alike (the MAC half was once caught live: a restart reissued
// the very first MAC).
func TestService_NewServiceRebuildsPoolsFromExistingResources(t *testing.T) {
	ctx := context.Background()
	etcdClient := resourcetest.Client(t)
	svc1, err := NewService(ctx, etcdClient, &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	netID := carveNetwork(t, ctx, svc1, "tenant-a")
	sn := mustCreateAndAllocateSubnet(t, ctx, svc1, "tenant-a", "sn1", SubnetSpec{NetworkID: netID, Zone: "zone-a"})
	iface := mustCreateAndAllocateNetworkInterface(t, ctx, svc1, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)

	svc2, err := NewService(ctx, etcdClient, &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sn2 := mustCreateAndAllocateSubnet(t, ctx, svc2, "tenant-a", "sn2", SubnetSpec{NetworkID: netID, Zone: "zone-a"})
	if sn2.Status.Values["vlan_id"] == sn.Status.Values["vlan_id"] {
		t.Fatalf("restarted process reissued vlan_id %d", sn2.Status.Values["vlan_id"])
	}
	if c1, _ := sn.Status.IPv4(); func() bool { c2, _ := sn2.Status.IPv4(); return c1 == c2 }() {
		t.Fatalf("restarted process re-carved CIDR %s", c1)
	}
	iface2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc2, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	if iface2.Status.IPAddress == iface.Status.IPAddress || iface2.Status.MACAddress == iface.Status.MACAddress {
		t.Fatalf("restarted process reissued ip/mac: %+v vs %+v", iface2.Status, iface.Status)
	}
}

func TestService_CreateNetworkInterfaceRejectsUnknownSubnet(t *testing.T) {
	svc, ctx := newTestService(t)
	if _, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: "subnet-nope"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("got %v, want ErrValidation", err)
	}
	if _, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("no subnet and no network: got %v, want ErrValidation", err)
	}
}

func TestService_CreateNetworkInterfaceRejectsOtherTenantsSubnet(t *testing.T) {
	svc, ctx := newTestService(t)
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", ""))
	if _, err := svc.CreateNetworkInterface(ctx, "tenant-b", "nic", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}); !errors.Is(err, ErrValidation) {
		t.Fatalf("another tenant's private Network's Subnet: got %v, want ErrValidation", err)
	}
}

func TestService_CreateNetworkInterfaceGoesReadyWithAllocatedIPMAC(t *testing.T) {
	svc, ctx := newTestService(t)
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", "10.0.1.1"))
	cidr, gw := sn.Status.IPv4()

	n1 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	if n1.Status.Phase != NetworkInterfacePhaseReady || n1.Status.MACAddress == "" || n1.Status.SubnetID != sn.Meta.ID {
		t.Fatalf("NIC not Ready with a MAC and subnet: %+v", n1.Status)
	}
	_, ipnet, _ := net.ParseCIDR(cidr)
	if ip := net.ParseIP(n1.Status.IPAddress); ip == nil || !ipnet.Contains(ip) || n1.Status.IPAddress == gw {
		t.Fatalf("ip %q: want inside %s and not the gateway %s", n1.Status.IPAddress, cidr, gw)
	}
	n2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	if n2.Status.MACAddress == n1.Status.MACAddress || n2.Status.IPAddress == n1.Status.IPAddress {
		t.Fatal("expected distinct MAC and IP")
	}
}

// TestService_SweepOrphanedNetworkInterfacesDeletesOnlyMissingVMs: parent
// Get -> NotFound means delete self (docs/specs/network.md
// "NetworkInterfaceのオーファンGC").
func TestService_SweepOrphanedNetworkInterfacesDeletesOnlyMissingVMs(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, &FakeVirtualMachineClient{Existing: map[string]bool{"vm-exists": true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", ""))
	live, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic-live", NetworkInterfaceSpec{VMID: "vm-exists", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic-orphan", NetworkInterfaceSpec{VMID: "vm-gone", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	svc.sweepOrphanedNetworkInterfaces(ctx)
	if _, err := svc.GetNetworkInterface(ctx, "tenant-a", live.Meta.ID); err != nil {
		t.Fatalf("live NIC deleted: %v", err)
	}
	if _, err := svc.GetNetworkInterface(ctx, "tenant-a", orphan.Meta.ID); !errors.Is(err, ErrNetworkInterfaceNotFound) {
		t.Fatalf("orphan NIC still there: %v", err)
	}
}

func TestService_CreateNetworkInterfaceReportsIPPoolExhausted(t *testing.T) {
	svc, ctx := newTestService(t)
	// /30 has exactly 2 usable host addresses; with .1 as the gateway, one.
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/30", "10.0.1.1"))
	n1 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	if n1.Status.Phase != NetworkInterfacePhaseReady {
		t.Fatalf("first NIC: %s", n1.Status.Phase)
	}
	n2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	if n2.Status.Phase != NetworkInterfacePhasePending {
		t.Fatalf("second NIC: phase %s, want Pending", n2.Status.Phase)
	}

	// Deleting n1 frees its IP -- via the reconciler's watch observing the
	// Deleted event (releaseNetworkInterface), not the Delete call itself
	// -- after which the retry sweep lets n2 through.
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go svc.watchPendingNetworkInterfaces(watchCtx)
	if err := svc.DeleteNetworkInterface(ctx, "tenant-a", n1.Meta.ID); err != nil {
		t.Fatal(err)
	}
	var retried *NetworkInterface
	for deadline := time.Now().Add(2 * time.Second); ; {
		svc.retryPendingNetworkInterfaces(ctx)
		retried, _ = svc.GetNetworkInterface(ctx, "tenant-a", n2.Meta.ID)
		if retried.Status.Phase == NetworkInterfacePhaseReady || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if retried.Status.Phase != NetworkInterfacePhaseReady || retried.Status.IPAddress != n1.Status.IPAddress {
		t.Fatalf("n2 = %+v, want Ready with n1's freed %s", retried.Status, n1.Status.IPAddress)
	}
}
