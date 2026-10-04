package compute

import (
	"errors"
	"testing"
	"time"
)

// shrinkNetifAllocationPoll overrides the package-level poll interval/
// timeout vars so a test exercising the still-Pending-at-timeout path
// doesn't actually have to wait netifAllocationPollTimeout's real-world
// duration.
func shrinkNetifAllocationPoll(t *testing.T) {
	t.Helper()
	oldInterval, oldTimeout := netifAllocationPollInterval, netifAllocationPollTimeout
	netifAllocationPollInterval = time.Millisecond
	netifAllocationPollTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		netifAllocationPollInterval, netifAllocationPollTimeout = oldInterval, oldTimeout
	})
}

func TestCreateNetworkInterfacesResolvesFullWiringInfo(t *testing.T) {
	ctx := t.Context()
	subnetClient := &FakeSubnetClient{Labels: map[string]string{"vpc.example.com/id": "vpc-1"}}
	netifClient := &FakeNetworkInterfaceClient{}

	infos, err := (&Service{subnetClient: subnetClient, netifClient: netifClient}).createNetworkInterfaces(ctx, "tenant-a", "vm-1", "zone-a", []NetworkAttachment{
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
	if info.CIDR != "10.0.0.0/24" || info.GatewayIP != "10.0.0.1" || info.Attach.SubnetValues["vlan_id"] != 1 || info.Attach.NetworkID != "network-1" {
		t.Fatalf("CIDR/GatewayIP/allocated values not resolved from the Subnet: %+v", info)
	}
	if info.SubnetID != "subnet-1" || info.Zone != "zone-a" || info.SubnetLabels["vpc.example.com/id"] != "vpc-1" {
		t.Fatalf("SubnetID/Zone/SubnetLabels not resolved: %+v", info)
	}
	if !info.Primary {
		t.Fatal("Primary did not carry through from the NetworkAttachment")
	}
}

// A NetworkInterface whose IP allocation hasn't succeeded yet (Subnet pool
// exhausted) must still be returned -- for VirtualMachineStatus.
// InterfaceRefs -- just without CIDR/GatewayIP/Attach, which compute-agent
// uses to decide whether it can wire a real tap device for it at all (see
// buildNetIfaces in internal/compute-agent/agent.go).
func TestCreateNetworkInterfacesLeavesUnallocatedInterfaceUnwired(t *testing.T) {
	shrinkNetifAllocationPoll(t)
	ctx := t.Context()
	subnetClient := &FakeSubnetClient{}
	netifClient := &FakeNetworkInterfaceClient{Pending: true}

	infos, err := (&Service{subnetClient: subnetClient, netifClient: netifClient}).createNetworkInterfaces(ctx, "tenant-a", "vm-1", "zone-a", []NetworkAttachment{
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

// TestCreateNetworkInterfacesWaitsForAsyncAllocation is the regression test
// for the real bug this poll fixes (2026-09-13, see
// docs/architecture.md「未決事項」#2 and the kyuusha_netif_ip_allocation_race
// memory note): Create's real network-side implementation always returns a
// NetworkInterface Pending with no IP yet, and allocation only completes
// slightly later (network-reconciler's own async loop). Without polling,
// createNetworkInterfaces used to just take whatever Create returned at
// that instant -- reliably losing this race and booting VMs with no real
// tap wired at all. FakeNetworkInterfaceClient{ReadyAfterGets: 2} simulates
// "Pending for the first 2 Gets, Ready on the 3rd" to prove the poll picks
// up an allocation that completes moments after Create, not just one that
// was already done by the time Create returned.
func TestCreateNetworkInterfacesWaitsForAsyncAllocation(t *testing.T) {
	ctx := t.Context()
	subnetClient := &FakeSubnetClient{}
	netifClient := &FakeNetworkInterfaceClient{ReadyAfterGets: 2}

	infos, err := (&Service{subnetClient: subnetClient, netifClient: netifClient}).createNetworkInterfaces(ctx, "tenant-a", "vm-1", "zone-a", []NetworkAttachment{
		{SubnetID: "subnet-1", Primary: true},
	})
	if err != nil {
		t.Fatalf("createNetworkInterfaces: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len(infos) = %d, want 1", len(infos))
	}
	info := infos[0]
	if info.IPAddress != "10.0.0.5" || info.MACAddress != "02:00:00:00:00:01" {
		t.Fatalf("expected the poll to pick up the allocation that completed after Create, got %+v", info)
	}
	if info.CIDR == "" || info.GatewayIP == "" {
		t.Fatalf("Subnet wiring info must be resolved once IPAddress is known: %+v", info)
	}
}

func TestValidateNetworkInterfaces_ZoneRules(t *testing.T) {
	ctx := t.Context()
	s := &Service{subnetClient: &FakeSubnetClient{}, NetworkClient: &FakeNetworkClient{}}

	if _, err := s.validateNetworkInterfaces(ctx, "tenant-a", "", []NetworkAttachment{{NetworkID: "network-1"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Network-only attachment without a zone: got %v, want ErrValidation", err)
	}
	zone, err := s.validateNetworkInterfaces(ctx, "tenant-a", "zone-a", []NetworkAttachment{{NetworkID: "network-1"}})
	if err != nil || zone != "zone-a" {
		t.Fatalf("Network-only attachment with zone-a: zone %q, err %v", zone, err)
	}
	zone, err = s.validateNetworkInterfaces(ctx, "tenant-a", "", []NetworkAttachment{{SubnetID: "subnet-1"}, {NetworkID: "network-1"}})
	if err != nil || zone != "zone-a" {
		t.Fatalf("zone taken from the pinned Subnet: zone %q, err %v", zone, err)
	}
	if _, err := s.validateNetworkInterfaces(ctx, "tenant-a", "zone-b", []NetworkAttachment{{SubnetID: "subnet-1"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("pinned Subnet in another zone than the VM's: got %v, want ErrValidation", err)
	}
	if _, err := s.validateNetworkInterfaces(ctx, "tenant-a", "zone-a", []NetworkAttachment{{NetworkID: "network-missing"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown network: got %v, want ErrValidation", err)
	}
	if _, err := (&Service{subnetClient: &FakeSubnetClient{}}).validateNetworkInterfaces(ctx, "tenant-a", "zone-a", []NetworkAttachment{{NetworkID: "network-1"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("no network client configured: got %v, want ErrValidation", err)
	}
}

func TestCreateNetworkInterfaces_AttachInfoFromNetwork(t *testing.T) {
	ctx := t.Context()
	s := &Service{subnetClient: &FakeSubnetClient{}, netifClient: &FakeNetworkInterfaceClient{}, NetworkClient: &FakeNetworkClient{}}
	infos, err := s.createNetworkInterfaces(ctx, "tenant-a", "vm-1", "zone-a", []NetworkAttachment{{NetworkID: "network-1", Primary: true}})
	if err != nil {
		t.Fatal(err)
	}
	a := infos[0].Attach
	if infos[0].SubnetID != "subnet-1" || a.NetworkValues["route_target"] != 65001 || a.NetworkLabels["k"] != "v" || a.SubnetValues["vlan_id"] != 1 {
		t.Fatalf("info = %+v", infos[0])
	}
}
