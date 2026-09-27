package network

import (
	"context"
	"errors"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestService_ValidateCrossTenantRulesRejectsUnsharedOverlap(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenantA, tenantB = "tenant-a", "tenant-b"
	mustCreateAndAllocateSubnet(t, ctx, svc, tenantB, "subnet-b", SubnetSpec{Zone: "zone-a", CIDR: "10.5.0.0/24"})

	// tenantA tries to allow-list tenantB's Subnet CIDR without consent.
	rules := []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "10.5.0.0/24", Action: "allow"}}
	if err := svc.validateCrossTenantRules(ctx, tenantA, rules); !errors.Is(err, ErrValidation) {
		t.Fatalf("got %v, want ErrValidation", err)
	}
}

func TestService_ValidateCrossTenantRulesAllowsSharedOverlap(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenantA, tenantB = "tenant-a", "tenant-b"
	mustCreateAndAllocateSubnet(t, ctx, svc, tenantB, "subnet-b", SubnetSpec{
		Zone: "zone-a", CIDR: "10.5.0.0/24", SharedWithTenantIDs: []string{tenantA},
	})

	rules := []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "10.5.0.0/24", Action: "allow"}}
	if err := svc.validateCrossTenantRules(ctx, tenantA, rules); err != nil {
		t.Fatalf("validateCrossTenantRules: %v", err)
	}
}

func TestService_ValidateCrossTenantRulesIgnoresDenyAndOwnTenant(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenantA, tenantB = "tenant-a", "tenant-b"
	mustCreateAndAllocateSubnet(t, ctx, svc, tenantB, "subnet-b", SubnetSpec{Zone: "zone-a", CIDR: "10.5.0.0/24"})
	mustCreateAndAllocateSubnet(t, ctx, svc, tenantA, "subnet-a", SubnetSpec{Zone: "zone-a", CIDR: "10.6.0.0/24"})

	// A deny rule reaching into tenantB's Subnet needs no consent.
	deny := []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "10.5.0.0/24", Action: "deny"}}
	if err := svc.validateCrossTenantRules(ctx, tenantA, deny); err != nil {
		t.Fatalf("deny rule should never require consent: %v", err)
	}

	// An allow rule reaching into the caller's own tenant's Subnet is fine.
	own := []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "10.6.0.0/24", Action: "allow"}}
	if err := svc.validateCrossTenantRules(ctx, tenantA, own); err != nil {
		t.Fatalf("allow rule reaching into the caller's own Subnet should never be rejected: %v", err)
	}
}

func TestService_CreateNetworkInterfaceRejectsUnsharedCrossTenantRule(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenantA, tenantB = "tenant-a", "tenant-b"
	mustCreateAndAllocateSubnet(t, ctx, svc, tenantB, "subnet-b", SubnetSpec{Zone: "zone-a", CIDR: "10.5.0.0/24"})
	subnetA := mustCreateAndAllocateSubnet(t, ctx, svc, tenantA, "subnet-a", SubnetSpec{Zone: "zone-a", CIDR: "10.6.0.0/24"})

	spec := NetworkInterfaceSpec{
		VMID: "vm-1", SubnetID: subnetA.Meta.ID,
		IngressRules: []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "10.5.0.0/24", Action: "allow"}},
	}
	if _, err := svc.CreateNetworkInterface(ctx, tenantA, "netif-1", spec); !errors.Is(err, ErrValidation) {
		t.Fatalf("got %v, want ErrValidation", err)
	}
}

func TestService_UpdateFirewallRulesRejectsUnsharedCrossTenantRule(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenantA, tenantB = "tenant-a", "tenant-b"
	mustCreateAndAllocateSubnet(t, ctx, svc, tenantB, "subnet-b", SubnetSpec{Zone: "zone-a", CIDR: "10.5.0.0/24"})
	subnetA := mustCreateAndAllocateSubnet(t, ctx, svc, tenantA, "subnet-a", SubnetSpec{Zone: "zone-a", CIDR: "10.6.0.0/24"})
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenantA, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnetA.Meta.ID}, subnetA)

	bad := []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "10.5.0.0/24", Action: "allow"}}
	if _, err := svc.UpdateFirewallRules(ctx, tenantA, n.Meta.ID, bad, nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("got %v, want ErrValidation", err)
	}
}

func TestService_EffectiveFirewallRulesAddsMeshGroupSiblings(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	subnet1 := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.1.0.0/24", MeshGroup: "prod"})
	subnet2 := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-2", SubnetSpec{Zone: "zone-a", CIDR: "10.2.0.0/24", MeshGroup: "prod"})
	mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-3", SubnetSpec{Zone: "zone-a", CIDR: "10.3.0.0/24", MeshGroup: "other"})

	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenant, "netif-1",
		NetworkInterfaceSpec{
			VMID: "vm-1", SubnetID: subnet1.Meta.ID,
			IngressRules: []FirewallRule{{Protocol: "tcp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "allow"}},
		}, subnet1)

	ingress, egress, err := svc.EffectiveFirewallRules(ctx, n)
	if err != nil {
		t.Fatalf("EffectiveFirewallRules: %v", err)
	}

	if len(ingress) != 2 {
		t.Fatalf("ingress = %+v, want the declared rule plus exactly one mesh sibling (subnet-2, not subnet-3)", ingress)
	}
	if ingress[0].SourceCIDR != "0.0.0.0/0" {
		t.Fatalf("ingress[0] = %+v, want the declared rule preserved first", ingress[0])
	}
	if ingress[1].SourceCIDR != subnet2.Spec.CIDR || ingress[1].Action != "allow" {
		t.Fatalf("ingress[1] = %+v, want an implicit allow for mesh sibling subnet-2 (%s)", ingress[1], subnet2.Spec.CIDR)
	}
	for _, r := range ingress {
		if r.SourceCIDR == "10.3.0.0/24" {
			t.Fatalf("subnet-3 (different mesh_group) must not be added: %+v", ingress)
		}
	}

	if len(egress) != 1 || egress[0].SourceCIDR != subnet2.Spec.CIDR {
		t.Fatalf("egress = %+v, want exactly one implicit mesh sibling allow (no declared egress_rules)", egress)
	}

	// Declaring spec.IngressRules itself must not have been mutated by the
	// append inside EffectiveFirewallRules.
	if len(n.Spec.IngressRules) != 1 {
		t.Fatalf("n.Spec.IngressRules was mutated: %+v", n.Spec.IngressRules)
	}
}

func TestService_EffectiveFirewallRulesNoMeshGroupReturnsSpecAsIs(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	subnet := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.1.0.0/24"})
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenant, "netif-1",
		NetworkInterfaceSpec{
			VMID: "vm-1", SubnetID: subnet.Meta.ID,
			IngressRules: []FirewallRule{{Protocol: "tcp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "allow"}},
		}, subnet)

	ingress, egress, err := svc.EffectiveFirewallRules(ctx, n)
	if err != nil {
		t.Fatalf("EffectiveFirewallRules: %v", err)
	}
	if len(ingress) != 1 || len(egress) != 0 {
		t.Fatalf("ingress=%+v egress=%+v, want spec rules unchanged (no mesh_group set)", ingress, egress)
	}
}
