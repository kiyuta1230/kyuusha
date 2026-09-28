package network

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// mustCreateAndAllocateSubnet creates a Subnet and immediately drives it
// through the same vlan_id allocation attempt cmd/network-reconciler's
// watchPendingSubnets would make on its Added event -- CreateSubnet itself
// never attempts this anymore (see its doc comment: vlanPool is
// process-local state, unsafe to touch from a possibly multi-replica API
// handler), so a test that needs a Subnet actually Ready must trigger the
// attempt explicitly, standing in for the real reconciler process.
func mustCreateAndAllocateSubnet(t *testing.T, ctx context.Context, svc *Service, tenantID, name string, spec SubnetSpec) *Subnet {
	t.Helper()
	sn, err := svc.CreateSubnet(ctx, tenantID, name, spec)
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	svc.tryAllocateVLAN(ctx, sn)
	return sn
}

// mustCreateAndAllocateNetworkInterface mirrors
// mustCreateAndAllocateSubnet, for ip_address/mac_address allocation via
// tryAllocateIP.
func mustCreateAndAllocateNetworkInterface(t *testing.T, ctx context.Context, svc *Service, tenantID, name string, spec NetworkInterfaceSpec, subnet *Subnet) *NetworkInterface {
	t.Helper()
	n, err := svc.CreateNetworkInterface(ctx, tenantID, name, spec)
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
	svc.tryAllocateIP(ctx, n, subnet.Spec.CIDR, subnet.Spec.GatewayIP, subnet.Spec.AllocatableIPRanges)
	return n
}

func TestService_CreateSubnetValidatesSpec(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := svc.CreateSubnet(ctx, "tenant-a", "x", SubnetSpec{CIDR: "10.0.1.0/24"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for missing zone, got %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "y", SubnetSpec{Zone: "zone-a", CIDR: "not-a-cidr"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for bad cidr, got %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "z", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24", GatewayIP: "not-an-ip"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for bad gateway_ip, got %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "w", SubnetSpec{
		Zone: "zone-a", CIDR: "10.0.1.0/24", AllocatableIPRanges: []string{"10.0.2.10-10.0.2.20"},
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for an allocatable_ip_ranges entry outside the cidr, got %v", err)
	}
}

func TestService_CreateNetworkInterfaceRespectsAllocatableIPRanges(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", SubnetSpec{
		Zone: "zone-a", CIDR: "10.0.1.0/24", AllocatableIPRanges: []string{"10.0.1.10-10.0.1.10"},
	})

	n1 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	if n1.Status.Phase != NetworkInterfacePhaseReady || n1.Status.IPAddress != "10.0.1.10" {
		t.Fatalf("expected phase=Ready ip=10.0.1.10, got phase=%s ip=%q", n1.Status.Phase, n1.Status.IPAddress)
	}

	// The range only has one address, so a second NetworkInterface must be
	// Pending (exhausted), even though the rest of the /24 is untouched.
	n2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	if n2.Status.Phase != NetworkInterfacePhasePending {
		t.Fatalf("expected phase Pending (allocatable_ip_ranges exhausted), got %s", n2.Status.Phase)
	}
}

func TestService_CreateSubnetGoesReadyWithVLANID(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if sn.Status.Phase != SubnetPhaseReady {
		t.Fatalf("expected phase Ready, got %s", sn.Status.Phase)
	}
	if sn.Status.VLANID == 0 {
		t.Fatal("expected a non-zero vlan_id")
	}

	sn2 := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn2", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"})
	if sn2.Status.VLANID == sn.Status.VLANID {
		t.Fatal("expected distinct vlan_ids across Subnets in the same zone")
	}

	sn3 := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn3", SubnetSpec{Zone: "zone-b", CIDR: "10.0.3.0/24"})
	if sn3.Status.VLANID != sn.Status.VLANID {
		t.Fatalf("expected zone-b's pool to be independent of zone-a's (reuse the same first id), got %d vs %d", sn3.Status.VLANID, sn.Status.VLANID)
	}
}

func TestService_CreateSubnetIsIdempotentByName(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	first, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	second, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet (repeat): %v", err)
	}
	if first.Meta.ID != second.Meta.ID {
		t.Fatalf("expected the same Subnet back, got %s and %s", first.Meta.ID, second.Meta.ID)
	}
}

// TestService_CreateNeverAllocatesSynchronously proves the 2026-09-13
// behavior change this session's compute-reconciler split led to: unlike
// before, CreateSubnet/CreateNetworkInterface must never touch
// vlanPool/ipPool/nextMACOct themselves (see their doc comments) --
// vlan_id/ip_address/mac_address allocation only ever happens in
// cmd/network-reconciler (watchPendingSubnets/watchPendingNetworkInterfaces
// or the periodic retry sweep), so a Create() call by itself, with no
// reconciler running at all, must always return Pending with nothing
// allocated -- proving this API handler is now safe to run as any number
// of replicas without touching shared process-local pool state.
func TestService_CreateNeverAllocatesSynchronously(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if sn.Status.Phase != SubnetPhasePending || sn.Status.VLANID != 0 {
		t.Fatalf("CreateSubnet allocated synchronously: phase=%s vlan_id=%d, want Pending/0", sn.Status.Phase, sn.Status.VLANID)
	}

	// tryAllocateVLAN directly (standing in for the reconciler, bypassing
	// Run/Watch entirely) so a NetworkInterface below has a Ready Subnet to
	// reference -- CreateNetworkInterface's own synchronous-allocation
	// check is the thing under test, not this setup step.
	svc.tryAllocateVLAN(ctx, sn)

	n, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
	if n.Status.Phase != NetworkInterfacePhasePending || n.Status.IPAddress != "" || n.Status.MACAddress != "" {
		t.Fatalf("CreateNetworkInterface allocated synchronously: phase=%s ip=%q mac=%q, want Pending/empty/empty", n.Status.Phase, n.Status.IPAddress, n.Status.MACAddress)
	}
}

// TestService_NewServiceRebuildsPoolsFromExistingResources proves the real
// bug found 2026-09-13: vlanPool/ipPool/nextMACOct are all purely
// in-memory, so without rebuildPools, every restart of this process -- not
// just a hypothetical multi-replica scenario -- would forget every VLAN
// ID/IP/MAC address already allocated and could hand the exact same one
// out again (the MAC half of this was caught live, serendipitously, while
// verifying the VLAN/IP fix against the real playground stack: two
// NetworkInterfaces created before/after a real network-1 container
// restart both got the identical mac_address). Constructs two Services
// against the SAME etcd client/namespace (not resourcetest.Client(t)
// called twice, which would give each its own isolated namespace) to
// simulate a real restart: the second NewService call is the fresh process,
// the first Subnet/NetworkInterface it never itself created.
func TestService_NewServiceRebuildsPoolsFromExistingResources(t *testing.T) {
	ctx := context.Background()
	etcdClient := resourcetest.Client(t)

	svc1, err := NewService(ctx, etcdClient, &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService (first): %v", err)
	}
	sn := mustCreateAndAllocateSubnet(t, ctx, svc1, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	iface := mustCreateAndAllocateNetworkInterface(t, ctx, svc1, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)

	svc2, err := NewService(ctx, etcdClient, &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService (second, simulating a restart): %v", err)
	}

	sn2 := mustCreateAndAllocateSubnet(t, ctx, svc2, "tenant-a", "sn2", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"})
	if sn2.Status.VLANID == sn.Status.VLANID {
		t.Fatalf("got the same vlan_id (%d) as the pre-restart Subnet -- rebuildPools did not restore vlanPool's state", sn2.Status.VLANID)
	}

	iface2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc2, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	if iface2.Status.IPAddress == iface.Status.IPAddress {
		t.Fatalf("got the same ip_address (%s) as the pre-restart NetworkInterface -- rebuildPools did not restore ipPool's state", iface2.Status.IPAddress)
	}
	if iface2.Status.MACAddress == iface.Status.MACAddress {
		t.Fatalf("got the same mac_address (%s) as the pre-restart NetworkInterface -- rebuildPools did not restore nextMACOct's state", iface2.Status.MACAddress)
	}
}

func TestService_CreateSubnetReportsVlanPoolExhausted(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Drain zone-a's pool directly (allocating one Subnet per VLAN ID would
	// be needlessly slow); Create should then leave a Subnet Pending rather
	// than reject it -- capacity may free up later.
	for i := 0; i < maxVLANID; i++ {
		if _, ok := svc.vlans.allocate("zone-a"); !ok {
			t.Fatalf("pool unexpectedly exhausted after %d allocations", i)
		}
	}

	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if sn.Status.Phase != SubnetPhasePending {
		t.Fatalf("expected phase Pending on pool exhaustion, got %s", sn.Status.Phase)
	}
	found := false
	for _, c := range sn.Status.Conditions {
		if c.Type == "VlanPoolExhausted" && c.Status == resource.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a VlanPoolExhausted condition, got %+v", sn.Status.Conditions)
	}

	// A different zone's pool is untouched.
	sn2 := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn2", SubnetSpec{Zone: "zone-b", CIDR: "10.0.2.0/24"})
	if sn2.Status.Phase != SubnetPhaseReady {
		t.Fatalf("expected zone-b's Subnet to go Ready, got %s", sn2.Status.Phase)
	}

	// Freeing one ID in zone-a and retrying the sweep lets sn1 through.
	svc.vlans.release("zone-a", 1)
	svc.retryPendingSubnets(ctx)
	retried, err := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	if err != nil {
		t.Fatalf("GetSubnet: %v", err)
	}
	if retried.Status.Phase != SubnetPhaseReady {
		t.Fatalf("expected sn1 to go Ready after the retry sweep, got %s", retried.Status.Phase)
	}
}

func TestService_CreateNetworkInterfaceRejectsUnknownSubnet(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	_, err = svc.CreateNetworkInterface(ctx, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: "subnet-does-not-exist"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for unknown subnet_id, got %v", err)
	}
}

func TestService_CreateNetworkInterfaceRejectsOtherTenantsSubnet(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}

	_, err = svc.CreateNetworkInterface(ctx, "tenant-b", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for a default-Private, unshared subnet, got %v", err)
	}
}

// TestService_CreateNetworkInterfaceRespectsVisibility exercises
// getSubnetForInterface's cross-tenant resolution + subnetUsableBy's gate,
// mirroring internal/image's Visibility/SharedWithTenantIDs semantics
// exactly (see docs/specs/network.md「spec.visibility」).
func TestService_CreateNetworkInterfaceRespectsVisibility(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	private := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn-private", SubnetSpec{
		Zone: "zone-a", CIDR: "10.0.1.0/24",
		Visibility: SubnetVisibilityPrivate, SharedWithTenantIDs: []string{"tenant-b"},
	})

	if _, err := svc.CreateNetworkInterface(ctx, "tenant-b", "nic-b", NetworkInterfaceSpec{VMID: "vm-b", SubnetID: private.Meta.ID}); err != nil {
		t.Fatalf("expected tenant-b (listed in shared_with_tenant_ids) to attach, got %v", err)
	}
	if _, err := svc.CreateNetworkInterface(ctx, "tenant-c", "nic-c", NetworkInterfaceSpec{VMID: "vm-c", SubnetID: private.Meta.ID}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for tenant-c (not listed), got %v", err)
	}

	public := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn-public", SubnetSpec{
		Zone: "zone-a", CIDR: "10.0.2.0/24", Visibility: SubnetVisibilityPublic,
	})
	if _, err := svc.CreateNetworkInterface(ctx, "tenant-c", "nic-c2", NetworkInterfaceSpec{VMID: "vm-c2", SubnetID: public.Meta.ID}); err != nil {
		t.Fatalf("expected any tenant to attach to a Public subnet, got %v", err)
	}
}

// TestService_CreateSubnetUniqueCIDR covers unique_cidr's cross-tenant CIDR
// overlap rejection, and confirms unique_cidr=false (the default) keeps the
// pre-existing "overlap is fine, different VRF" behavior.
func TestService_CreateSubnetUniqueCIDR(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := svc.CreateSubnet(ctx, "tenant-a", "pub1", SubnetSpec{
		Zone: "zone-a", CIDR: "203.0.113.0/28", UniqueCidr: true,
	}); err != nil {
		t.Fatalf("CreateSubnet(pub1): %v", err)
	}

	// Same tenant, overlapping unique_cidr Subnet: still rejected (unlike
	// shared_with_tenant_ids, there's no same-tenant exemption here).
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "pub2", SubnetSpec{
		Zone: "zone-a", CIDR: "203.0.113.0/29", UniqueCidr: true,
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for an overlapping unique_cidr Subnet (same tenant), got %v", err)
	}

	// Different tenant, overlapping unique_cidr Subnet: rejected.
	if _, err := svc.CreateSubnet(ctx, "tenant-b", "pub3", SubnetSpec{
		Zone: "zone-b", CIDR: "203.0.113.8/29", UniqueCidr: true,
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for an overlapping unique_cidr Subnet (different tenant), got %v", err)
	}

	// Same CIDR, unique_cidr=false: allowed, same as today's private-Subnet
	// overlap behavior.
	if _, err := svc.CreateSubnet(ctx, "tenant-b", "priv1", SubnetSpec{
		Zone: "zone-b", CIDR: "203.0.113.0/28",
	}); err != nil {
		t.Fatalf("expected overlap to be allowed when unique_cidr=false, got %v", err)
	}
}

func TestService_CreateNetworkInterfaceGoesReadyWithAllocatedIPMAC(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"})

	n1 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	if n1.Status.Phase != NetworkInterfacePhaseReady {
		t.Fatalf("expected phase Ready, got %s", n1.Status.Phase)
	}
	if n1.Status.MACAddress == "" {
		t.Fatal("expected a mac_address")
	}
	ip := net.ParseIP(n1.Status.IPAddress)
	_, cidr, _ := net.ParseCIDR(sn.Spec.CIDR)
	if ip == nil || !cidr.Contains(ip) {
		t.Fatalf("expected ip_address inside %s, got %q", sn.Spec.CIDR, n1.Status.IPAddress)
	}
	if n1.Status.IPAddress == sn.Spec.GatewayIP {
		t.Fatalf("expected the gateway_ip to never be handed out, got it as ip_address")
	}

	n2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	if n2.Status.MACAddress == n1.Status.MACAddress {
		t.Fatal("expected distinct mac_addresses across NetworkInterfaces")
	}
	if n2.Status.IPAddress == n1.Status.IPAddress {
		t.Fatal("expected distinct ip_addresses across NetworkInterfaces on the same Subnet")
	}
}

// TestService_SweepOrphanedNetworkInterfacesDeletesOnlyMissingVMs proves
// docs/architecture.md's orphan-GC detection logic (parent Get -> NotFound
// means delete self) against a real embedded etcd, using a
// FakeVirtualMachineClient that knows about vm-exists but not vm-gone --
// see docs/specs/network.md "NetworkInterfaceのオーファンGC" for the leak
// this closes (VM Delete never touches its NetworkInterfaces today).
func TestService_SweepOrphanedNetworkInterfacesDeletesOnlyMissingVMs(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, &FakeVirtualMachineClient{Existing: map[string]bool{"vm-exists": true}}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"})
	live, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic-live", NetworkInterfaceSpec{VMID: "vm-exists", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface(live): %v", err)
	}
	orphan, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic-orphan", NetworkInterfaceSpec{VMID: "vm-gone", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface(orphan): %v", err)
	}

	svc.sweepOrphanedNetworkInterfaces(ctx)

	if _, err := svc.GetNetworkInterface(ctx, "tenant-a", live.Meta.ID); err != nil {
		t.Fatalf("live NetworkInterface (vm-exists) was deleted: %v", err)
	}
	if _, err := svc.GetNetworkInterface(ctx, "tenant-a", orphan.Meta.ID); !errors.Is(err, ErrNetworkInterfaceNotFound) {
		t.Fatalf("orphaned NetworkInterface (vm-gone) still exists: err=%v, want ErrNetworkInterfaceNotFound", err)
	}
}

func TestService_CreateNetworkInterfaceReportsIPPoolExhausted(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// /30 has exactly 2 usable host addresses.
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/30"})

	n1 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	if n1.Status.Phase != NetworkInterfacePhaseReady {
		t.Fatalf("CreateNetworkInterface (1st): phase=%v", n1.Status.Phase)
	}
	n2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	if n2.Status.Phase != NetworkInterfacePhaseReady {
		t.Fatalf("CreateNetworkInterface (2nd): phase=%v", n2.Status.Phase)
	}

	n3 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic3", NetworkInterfaceSpec{VMID: "vm-3", SubnetID: sn.Meta.ID}, sn)
	if n3.Status.Phase != NetworkInterfacePhasePending {
		t.Fatalf("expected phase Pending on IP pool exhaustion, got %s", n3.Status.Phase)
	}

	// Deleting n1 frees its IP; the retry sweep should then let n3 through.
	if err := svc.DeleteNetworkInterface(ctx, "tenant-a", n1.Meta.ID); err != nil {
		t.Fatalf("DeleteNetworkInterface: %v", err)
	}
	svc.retryPendingNetworkInterfaces(ctx)
	retried, err := svc.GetNetworkInterface(ctx, "tenant-a", n3.Meta.ID)
	if err != nil {
		t.Fatalf("GetNetworkInterface: %v", err)
	}
	if retried.Status.Phase != NetworkInterfacePhaseReady {
		t.Fatalf("expected n3 to go Ready after the retry sweep, got %s", retried.Status.Phase)
	}
	if retried.Status.IPAddress != n1.Status.IPAddress {
		t.Fatalf("expected n3 to reuse n1's freed ip %s, got %s", n1.Status.IPAddress, retried.Status.IPAddress)
	}
}
