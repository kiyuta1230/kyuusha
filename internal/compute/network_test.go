package compute

import "testing"

func TestCreateNetworkInterfacesResolvesFullWiringInfo(t *testing.T) {
	ctx := t.Context()
	subnetClient := &FakeSubnetClient{}
	netifClient := &FakeNetworkInterfaceClient{}

	infos, err := createNetworkInterfaces(ctx, subnetClient, netifClient, "tenant-a", "vm-1", []NetworkAttachment{
		{SubnetID: "subnet-1", Primary: true},
	})
	if err != nil {
		t.Fatalf("createNetworkInterfaces: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1", len(infos))
	}
	info := infos[0]
	if info.IfaceID != "netif-iface-vm-1-0" {
		t.Fatalf("IfaceID = %q, want the deterministic iface-<vm-id>-<index> name", info.IfaceID)
	}
	if info.IPAddress != "10.0.0.5" || info.MACAddress != "02:00:00:00:00:01" {
		t.Fatalf("IPAddress/MACAddress not resolved from the NetworkInterface: %+v", info)
	}
	if info.CIDR != "10.0.0.0/24" || info.GatewayIP != "10.0.0.1" || info.VLANID != 1 {
		t.Fatalf("CIDR/GatewayIP/VLANID not resolved from the Subnet: %+v", info)
	}
	if !info.Primary {
		t.Fatal("Primary did not carry through from the NetworkAttachment")
	}
}

// A NetworkInterface whose IP allocation hasn't succeeded yet (Subnet pool
// exhausted) must still be returned -- for VirtualMachineStatus.
// InterfaceRefs -- just without CIDR/GatewayIP/VLANID, which compute-agent
// uses to decide whether it can wire a real tap device for it at all (see
// buildNetIfaces in internal/compute-agent/agent.go).
func TestCreateNetworkInterfacesLeavesUnallocatedInterfaceUnwired(t *testing.T) {
	ctx := t.Context()
	subnetClient := &FakeSubnetClient{}
	netifClient := &FakeNetworkInterfaceClient{Pending: true}

	infos, err := createNetworkInterfaces(ctx, subnetClient, netifClient, "tenant-a", "vm-1", []NetworkAttachment{
		{SubnetID: "subnet-1"},
	})
	if err != nil {
		t.Fatalf("createNetworkInterfaces: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1", len(infos))
	}
	info := infos[0]
	if info.IfaceID == "" {
		t.Fatal("IfaceID must still be populated for a Pending NetworkInterface")
	}
	if info.IPAddress != "" || info.CIDR != "" {
		t.Fatalf("expected no IPAddress/CIDR for a Pending NetworkInterface, got %+v", info)
	}
}
