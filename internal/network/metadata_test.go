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
	sn, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "sn", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"}, md)
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
	if _, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "sn-bad", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"},
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
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	forged, _ := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	realVLAN := forged.Status.VLANID
	forged.Status.VLANID = 999
	forged.Meta.Labels = map[string]string{"k": "v"}
	out, err := svc.UpdateSubnet(ctx, forged)
	if err != nil {
		t.Fatalf("UpdateSubnet: %v", err)
	}
	if out.Status.VLANID != realVLAN || out.Meta.Labels["k"] != "v" {
		t.Fatalf("after Update vlan_id=%d labels=%v, want vlan_id %d kept and the label applied", out.Status.VLANID, out.Meta.Labels, realVLAN)
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
