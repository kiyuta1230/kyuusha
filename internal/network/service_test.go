package network

import (
	"context"
	"errors"
	"net"
	"testing"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

func TestService_CreateSubnetValidatesSpec(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

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
	svc := NewService()

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{
		Zone: "zone-a", CIDR: "10.0.1.0/24", AllocatableIPRanges: []string{"10.0.1.10-10.0.1.10"},
	})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}

	n1, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID})
	if err != nil || n1.Status.Phase != NetworkInterfacePhaseReady || n1.Status.IPAddress != "10.0.1.10" {
		t.Fatalf("expected phase=Ready ip=10.0.1.10, got phase=%s ip=%q err=%v", n1.Status.Phase, n1.Status.IPAddress, err)
	}

	// The range only has one address, so a second NetworkInterface must be
	// Pending (exhausted), even though the rest of the /24 is untouched.
	n2, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
	if n2.Status.Phase != NetworkInterfacePhasePending {
		t.Fatalf("expected phase Pending (allocatable_ip_ranges exhausted), got %s", n2.Status.Phase)
	}
}

func TestService_CreateSubnetGoesReadyWithVLANID(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if sn.Status.Phase != SubnetPhaseReady {
		t.Fatalf("expected phase Ready, got %s", sn.Status.Phase)
	}
	if sn.Status.VLANID == 0 {
		t.Fatal("expected a non-zero vlan_id")
	}

	sn2, err := svc.CreateSubnet(ctx, "tenant-a", "sn2", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if sn2.Status.VLANID == sn.Status.VLANID {
		t.Fatal("expected distinct vlan_ids across Subnets in the same zone")
	}

	sn3, err := svc.CreateSubnet(ctx, "tenant-a", "sn3", SubnetSpec{Zone: "zone-b", CIDR: "10.0.3.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if sn3.Status.VLANID != sn.Status.VLANID {
		t.Fatalf("expected zone-b's pool to be independent of zone-a's (reuse the same first id), got %d vs %d", sn3.Status.VLANID, sn.Status.VLANID)
	}
}

func TestService_CreateSubnetIsIdempotentByName(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

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

func TestService_CreateSubnetReportsVlanPoolExhausted(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	// Drain zone-a's pool directly (allocating one Subnet per VLAN ID would
	// be needlessly slow); Create should then leave a Subnet Pending rather
	// than reject it -- capacity may free up later.
	for i := 0; i < maxVLANID; i++ {
		if _, ok := svc.vlans.allocate("zone-a"); !ok {
			t.Fatalf("pool unexpectedly exhausted after %d allocations", i)
		}
	}

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
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
	sn2, err := svc.CreateSubnet(ctx, "tenant-a", "sn2", SubnetSpec{Zone: "zone-b", CIDR: "10.0.2.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet (zone-b): %v", err)
	}
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
	svc := NewService()

	_, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: "subnet-does-not-exist"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for unknown subnet_id, got %v", err)
	}
}

func TestService_CreateNetworkInterfaceRejectsOtherTenantsSubnet(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}

	_, err = svc.CreateNetworkInterface(ctx, "tenant-b", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for a different tenant's subnet, got %v", err)
	}
}

func TestService_CreateNetworkInterfaceGoesReadyWithAllocatedIPMAC(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}

	n1, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
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

	n2, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
	if n2.Status.MACAddress == n1.Status.MACAddress {
		t.Fatal("expected distinct mac_addresses across NetworkInterfaces")
	}
	if n2.Status.IPAddress == n1.Status.IPAddress {
		t.Fatal("expected distinct ip_addresses across NetworkInterfaces on the same Subnet")
	}
}

func TestService_CreateNetworkInterfaceReportsIPPoolExhausted(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	// /30 has exactly 2 usable host addresses.
	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/30"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}

	n1, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID})
	if err != nil || n1.Status.Phase != NetworkInterfacePhaseReady {
		t.Fatalf("CreateNetworkInterface (1st): err=%v phase=%v", err, n1.Status.Phase)
	}
	n2, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID})
	if err != nil || n2.Status.Phase != NetworkInterfacePhaseReady {
		t.Fatalf("CreateNetworkInterface (2nd): err=%v phase=%v", err, n2.Status.Phase)
	}

	n3, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic3", NetworkInterfaceSpec{VMID: "vm-3", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface (3rd): %v", err)
	}
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
