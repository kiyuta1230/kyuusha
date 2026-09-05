package network

import (
	"context"
	"errors"
	"testing"
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
		t.Fatal("expected a non-zero mocked vlan_id")
	}

	sn2, err := svc.CreateSubnet(ctx, "tenant-a", "sn2", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if sn2.Status.VLANID == sn.Status.VLANID {
		t.Fatal("expected distinct mocked vlan_ids across Subnets")
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

func TestService_CreateNetworkInterfaceGoesReadyWithMockedIPMAC(t *testing.T) {
	ctx := context.Background()
	svc := NewService()

	sn, err := svc.CreateSubnet(ctx, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
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
		t.Fatal("expected a mocked mac_address")
	}

	n2, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID})
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
	if n2.Status.MACAddress == n1.Status.MACAddress {
		t.Fatal("expected distinct mocked mac_addresses across NetworkInterfaces")
	}
}
