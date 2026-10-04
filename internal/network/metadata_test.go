package network

import (
	"context"
	"errors"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestService_SubnetLabelsAndAnnotations covers meta.labels/annotations'
// lifecycle on Subnet: set at Create, replaced wholesale by Update, and
// validated on both paths (see resource.ValidateMetadata).
func TestService_SubnetLabelsAndAnnotations(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	md := resource.Metadata{
		Labels:      map[string]string{"vpc.example.com/id": "vpc-1"},
		Annotations: map[string]string{"example.com/note": "anything at all"},
	}
	sn, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "sn", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", ""), md)
	if err != nil {
		t.Fatalf("CreateSubnetWithMetadata: %v", err)
	}
	got, err := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	if err != nil {
		t.Fatalf("GetSubnet: %v", err)
	}
	if got.Meta.Labels["vpc.example.com/id"] != "vpc-1" || got.Meta.Annotations["example.com/note"] != "anything at all" {
		t.Fatalf("stored meta = %+v, want the labels/annotations given at Create", got.Meta)
	}

	got.Meta.Labels = map[string]string{"tier": "web"}
	updated, err := svc.UpdateSubnet(ctx, got)
	if err != nil {
		t.Fatalf("UpdateSubnet: %v", err)
	}
	if len(updated.Meta.Labels) != 1 || updated.Meta.Labels["tier"] != "web" {
		t.Fatalf("labels after Update = %v, want exactly {tier: web}", updated.Meta.Labels)
	}

	updated.Meta.Labels = map[string]string{"bad key": "x"}
	if _, err := svc.UpdateSubnet(ctx, updated); !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateSubnet with an invalid label key: got %v, want ErrValidation", err)
	}
	if _, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "sn-bad", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.2.0/24", ""),
		resource.Metadata{Labels: map[string]string{"k": "has space"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateSubnetWithMetadata with an invalid label value: got %v, want ErrValidation", err)
	}
}

// TestService_UpdateCannotForgeServerOwnedFields: the Update RPCs'
// service methods take only what callers may set -- a forged vlan_id would
// wire this tenant's VMs into another tenant's VLAN, a forged ip_address
// would defeat SNAP's anti-spoofing.
func TestService_UpdateCannotForgeServerOwnedFields(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn", SubnetSpec{NetworkID: carveNetwork(t, ctx, svc, "tenant-a"), Zone: "zone-a"})
	forged, _ := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	realVLAN := forged.Status.Values["vlan_id"]
	forged.Status.Values = map[string]int64{"vlan_id": 999}
	forged.Meta.Labels = map[string]string{"k": "v"}
	out, err := svc.UpdateSubnet(ctx, forged)
	if err != nil {
		t.Fatalf("UpdateSubnet: %v", err)
	}
	if out.Status.Values["vlan_id"] != realVLAN || out.Meta.Labels["k"] != "v" {
		t.Fatalf("after Update vlan_id=%d labels=%v, want vlan_id %d kept and the label applied", out.Status.Values["vlan_id"], out.Meta.Labels, realVLAN)
	}

	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, out)
	nf, _ := svc.GetNetworkInterface(ctx, "tenant-a", n.Meta.ID)
	realIP := nf.Status.IPAddress
	nf.Status.IPAddress = "10.0.1.200"
	nf.Status.Hypervisor = "forged-hv"
	nout, err := svc.UpdateNetworkInterface(ctx, nf)
	if err != nil {
		t.Fatalf("UpdateNetworkInterface: %v", err)
	}
	if nout.Status.IPAddress != realIP || nout.Status.Hypervisor != "" {
		t.Fatalf("after Update status=%+v, want ip %s kept and hypervisor empty", nout.Status, realIP)
	}
	nout.Spec.VMID = "vm-other"
	if _, err := svc.UpdateNetworkInterface(ctx, nout); !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateNetworkInterface changing vm_id: got %v, want ErrValidation", err)
	}
}

func TestService_UpdateSubnetRejectsAddressingChanges(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", "10.0.1.1"))
	for name, mutate := range map[string]func(*Subnet){
		"zone":       func(s *Subnet) { s.Spec.Zone = "zone-b" },
		"network_id": func(s *Subnet) { s.Spec.NetworkID = "network-other" },
		"cidr":       func(s *Subnet) { s.Spec.RequestedAddresses = []SubnetAddress{{CIDR: "10.0.9.0/24"}} },
		"gateway_ip": func(s *Subnet) { s.Spec.RequestedAddresses = []SubnetAddress{{CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.254"}} },
	} {
		cur, _ := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
		mutate(cur)
		if _, err := svc.UpdateSubnet(ctx, cur); !errors.Is(err, ErrValidation) {
			t.Errorf("changing %s: got %v, want ErrValidation", name, err)
		}
	}
	// Other spec fields stay updatable.
	cur, _ := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	cur.Spec.DNSServers = []string{"10.0.1.53"}
	if out, err := svc.UpdateSubnet(ctx, cur); err != nil || len(out.Spec.DNSServers) != 1 {
		t.Fatalf("changing dns_servers: %v, %+v", err, out)
	}
}
