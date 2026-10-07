package network

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func mustReserve(t *testing.T, ctx context.Context, svc *Service, tenantID, name string, spec IPReservationSpec) *IPReservation {
	t.Helper()
	r, err := svc.CreateIPReservation(ctx, tenantID, name, spec, resource.Metadata{})
	if err != nil {
		t.Fatalf("CreateIPReservation %s: %v", name, err)
	}
	if r.Status.Phase != IPReservationPhasePending {
		t.Fatalf("created %s as %s, want Pending", name, r.Status.Phase)
	}
	svc.tryAllocateReservation(ctx, r)
	return r
}

// TestIPReservation_SharesIPAMWithNICs: reservations and NICs draw from
// one pool -- never the same address -- and exhaustion leaves a
// reservation Pending until an address frees up.
func TestIPReservation_SharesIPAMWithNICs(t *testing.T) {
	ctx := context.Background()
	client := resourcetest.Client(t)
	svc, err := NewService(ctx, client, &FakeTenantClient{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	netID := testNetwork(t, ctx, svc, "tenant-a")
	// /29 with gateway .1: five usable addresses (.2-.6).
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.9.0.0/29", GatewayIP: "10.9.0.1"}}})

	seen := map[string]string{}
	take := func(what, addr string) {
		t.Helper()
		if addr == "" {
			t.Fatalf("%s got no address", what)
		}
		if prev, dup := seen[addr]; dup {
			t.Fatalf("%s got %s, already held by %s", what, addr, prev)
		}
		seen[addr] = what
	}
	r1 := mustReserve(t, ctx, svc, "tenant-a", "r1", IPReservationSpec{NetworkID: netID, Zone: "z"})
	if r1.Status.Phase != IPReservationPhaseReady || r1.Status.SubnetID != sn.Meta.ID || r1.Status.Zone != "z" || r1.Spec.SubnetID != "" {
		t.Fatalf("r1 = %+v", r1)
	}
	take("r1", r1.Status.Addresses[0])
	// A specific address, outside nothing in particular: .6.
	r2 := mustReserve(t, ctx, svc, "tenant-a", "r2", IPReservationSpec{SubnetID: sn.Meta.ID, RequestedAddresses: []string{"10.9.0.6"}})
	if r2.Status.Phase != IPReservationPhaseReady || !slices.Equal(r2.Status.Addresses, []string{"10.9.0.6"}) || r2.Spec.NetworkID != netID {
		t.Fatalf("r2 = %+v", r2)
	}
	take("r2", "10.9.0.6")
	for _, name := range []string{"n1", "n2", "n3"} {
		n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", name, NetworkInterfaceSpec{VMID: "vm-" + name, SubnetID: sn.Meta.ID}, sn)
		take(name, n.Status.IPAddress)
	}
	// The Subnet is now full: the next reservation waits.
	r3 := mustReserve(t, ctx, svc, "tenant-a", "r3", IPReservationSpec{NetworkID: netID, Zone: "z"})
	if r3.Status.Phase != IPReservationPhasePending || !hasTrueCondition(r3.Status.Conditions, "NoFreeAddress") {
		t.Fatalf("r3 on a full subnet = %+v", r3.Status)
	}
	// Deleting r1 returns its address (on the Deleted event); r3 gets it.
	freed := r1.Status.Addresses[0]
	if err := svc.DeleteIPReservation(ctx, "tenant-a", r1.Meta.ID); err != nil {
		t.Fatal(err)
	}
	svc.releaseIPReservation(*r1)
	svc.retryPendingIPReservations(ctx)
	r3, _ = svc.GetIPReservation(ctx, "tenant-a", r3.Meta.ID)
	if r3.Status.Phase != IPReservationPhaseReady || r3.Status.Addresses[0] != freed {
		t.Fatalf("r3 after r1's deletion = %+v, want %s", r3.Status, freed)
	}

	// A restart rebuilds the pool from etcd: reserved addresses stay taken.
	svc2, err := NewService(ctx, client, &FakeTenantClient{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cidr, gw := sn.Status.IPv4()
	if svc2.ips.allocateSpecific(sn.Meta.ID, cidr, gw, "10.9.0.6") {
		t.Fatal("a fresh process handed out r2's reserved address")
	}
}

func TestIPReservation_Validation(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := testNetwork(t, ctx, svc, "tenant-a")
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.9.1.0/24", GatewayIP: "10.9.1.1"}}})
	mustReserve(t, ctx, svc, "tenant-a", "taken", IPReservationSpec{SubnetID: sn.Meta.ID, RequestedAddresses: []string{"10.9.1.50"}})
	for name, spec := range map[string]IPReservationSpec{
		"nothing":                {},
		"network without zone":   {NetworkID: netID},
		"address without subnet": {NetworkID: netID, Zone: "z", RequestedAddresses: []string{"10.9.1.9"}},
		"ipv6":                   {SubnetID: sn.Meta.ID, RequestedAddresses: []string{"fd00::9"}},
		"two ipv4":               {SubnetID: sn.Meta.ID, RequestedAddresses: []string{"10.9.1.8", "10.9.1.9"}},
		"outside cidr":           {SubnetID: sn.Meta.ID, RequestedAddresses: []string{"10.9.2.9"}},
		"network address":        {SubnetID: sn.Meta.ID, RequestedAddresses: []string{"10.9.1.0"}},
		"gateway":                {SubnetID: sn.Meta.ID, RequestedAddresses: []string{"10.9.1.1"}},
		"already reserved":       {SubnetID: sn.Meta.ID, RequestedAddresses: []string{"10.9.1.50"}},
		"missing subnet":         {SubnetID: "subnet-nope"},
		"missing network":        {NetworkID: "network-nope", Zone: "z"},
		"other zone":             {SubnetID: sn.Meta.ID, Zone: "elsewhere"},
	} {
		if _, err := svc.CreateIPReservation(ctx, "tenant-a", "x-"+name, spec, resource.Metadata{}); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: got %v, want ErrValidation", name, err)
		}
	}
	// Another tenant can't reserve from a private Network; sharing it lets it.
	if _, err := svc.CreateIPReservation(ctx, "tenant-b", "b", IPReservationSpec{NetworkID: netID, Zone: "z"}, resource.Metadata{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("tenant-b on tenant-a's private network: %v", err)
	}
	n, _ := svc.GetNetwork(ctx, "tenant-a", netID)
	n.Spec.SharedWithTenantIDs = []string{"tenant-b"}
	if _, err := svc.UpdateNetwork(ctx, n); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateIPReservation(ctx, "tenant-b", "b", IPReservationSpec{NetworkID: netID, Zone: "z"}, resource.Metadata{}); err != nil {
		t.Fatalf("tenant-b on a network shared with it: %v", err)
	}
}

// TestIPReservation_Lifecycle: a reservation blocks its Subnet's and
// Network's deletion; a Finalizer holds it (and its address) until
// cleared; only meta can be updated.
func TestIPReservation_Lifecycle(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := testNetwork(t, ctx, svc, "tenant-a")
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.9.3.0/24"}}})
	r := mustReserve(t, ctx, svc, "tenant-a", "vip", IPReservationSpec{NetworkID: netID, Zone: "z"})

	if err := svc.DeleteSubnet(ctx, "tenant-a", sn.Meta.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("DeleteSubnet with a reservation: %v", err)
	}
	if err := svc.DeleteNetwork(ctx, "tenant-a", netID); !errors.Is(err, ErrInUse) {
		t.Fatalf("DeleteNetwork with a reservation: %v", err)
	}

	cur, _ := svc.GetIPReservation(ctx, "tenant-a", r.Meta.ID)
	cur.Meta.Labels = map[string]string{"vpc.example.com/role": "lb-vip"}
	cur.Meta.Finalizers = []resource.Finalizer{{Name: "vpc.example.com/lb"}}
	cur.Spec.RequestedAddresses = []string{"10.9.3.200"} // ignored: spec is fixed
	cur.Status.Addresses = []string{"10.9.3.200"}        // ignored: status is server-owned
	upd, err := svc.UpdateIPReservation(ctx, "tenant-a", cur)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Meta.Labels["vpc.example.com/role"] != "lb-vip" || len(upd.Spec.RequestedAddresses) != 0 || upd.Status.Addresses[0] != r.Status.Addresses[0] {
		t.Fatalf("after Update = %+v", upd)
	}
	if err := svc.DeleteIPReservation(ctx, "tenant-a", r.Meta.ID); err != nil {
		t.Fatal(err)
	}
	held, err := svc.GetIPReservation(ctx, "tenant-a", r.Meta.ID)
	if err != nil || held.Meta.DeletedAt == nil || held.Status.Addresses[0] != r.Status.Addresses[0] {
		t.Fatalf("held by its Finalizer: %+v, %v", held, err)
	}
	held.Meta.Finalizers = nil
	if _, err := svc.UpdateIPReservation(ctx, "tenant-a", held); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetIPReservation(ctx, "tenant-a", r.Meta.ID); !errors.Is(err, ErrIPReservationNotFound) {
		t.Fatalf("after clearing the Finalizer: %v", err)
	}
	if err := svc.DeleteSubnet(ctx, "tenant-a", sn.Meta.ID); err != nil {
		t.Fatalf("DeleteSubnet once the reservation is gone: %v", err)
	}
}

func TestIPReservation_Quota(t *testing.T) {
	ctx := context.Background()
	q := UnlimitedQuota()
	q.MaxIpReservations = 1
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: q}, nil)
	if err != nil {
		t.Fatal(err)
	}
	netID := testNetwork(t, ctx, svc, "tenant-a")
	mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.9.4.0/24"}}})
	r := mustReserve(t, ctx, svc, "tenant-a", "one", IPReservationSpec{NetworkID: netID, Zone: "z"})
	if _, err := svc.CreateIPReservation(ctx, "tenant-a", "two", IPReservationSpec{NetworkID: netID, Zone: "z"}, resource.Metadata{}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over max_ip_reservations=1: %v", err)
	}
	if err := svc.DeleteIPReservation(ctx, "tenant-a", r.Meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateIPReservation(ctx, "tenant-a", "two", IPReservationSpec{NetworkID: netID, Zone: "z"}, resource.Metadata{}); err != nil {
		t.Fatalf("after deleting one: %v", err)
	}
}
