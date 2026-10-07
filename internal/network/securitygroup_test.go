package network

import (
	"errors"
	"slices"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

func TestSecurityGroup_DefaultGroupOfNetwork(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := testNetwork(t, ctx, svc, "tenant-a")
	n, err := svc.GetNetwork(ctx, "tenant-a", netID)
	if err != nil {
		t.Fatal(err)
	}
	sgID := n.Status.DefaultSecurityGroupID
	if sgID == "" {
		t.Fatal("Ready Network has no default security group")
	}
	g, err := svc.GetSecurityGroup(ctx, "tenant-a", sgID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status.DefaultForNetworkID != netID || len(g.Spec.IngressRules) != 1 || g.Spec.IngressRules[0].Peer.NetworkID != netID ||
		!slices.ContainsFunc(g.Spec.EgressRules, func(r SecurityGroupRule) bool { return r.Peer.CIDR == "0.0.0.0/0" }) {
		t.Fatalf("default group = %+v", g)
	}
	// A NIC with no groups named gets the default one.
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.0/24"}}})
	nic := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic", NetworkInterfaceSpec{VMID: "vm", SubnetID: sn.Meta.ID}, sn)
	if !slices.Equal(nic.Spec.SecurityGroupIDs, []string{sgID}) {
		t.Fatalf("NIC groups = %v, want the default %s", nic.Spec.SecurityGroupIDs, sgID)
	}
	if err := svc.DeleteSecurityGroup(ctx, "tenant-a", sgID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete default group: got %v, want ErrInUse", err)
	}
	// Its rules may change, though.
	g.Spec.IngressRules = nil
	if _, err := svc.UpdateSecurityGroup(ctx, "tenant-a", g); err != nil {
		t.Fatalf("update default group: %v", err)
	}
	// The Network's Deleted event removes it.
	svc.deleteDefaultSecurityGroup(ctx, *n)
	if _, err := svc.GetSecurityGroup(ctx, "tenant-a", sgID); !errors.Is(err, ErrSecurityGroupNotFound) {
		t.Fatalf("after Network deletion: %v", err)
	}
}

func TestSecurityGroup_Validation(t *testing.T) {
	svc, ctx := newTestService(t)
	netA := testNetwork(t, ctx, svc, "tenant-a")
	other, err := svc.CreateSecurityGroup(ctx, "tenant-b", "b-only", SecurityGroupSpec{}, resource.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]SecurityGroupRule{
		"bad protocol":    {Protocol: "sctp", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}},
		"port on icmp":    {Protocol: "icmp", PortRange: "22", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}},
		"port on any":     {PortRange: "22", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}},
		"bad port":        {Protocol: "tcp", PortRange: "70000", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}},
		"no peer":         {Protocol: "tcp"},
		"two peers":       {Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0", NetworkID: netA}},
		"bad cidr":        {Peer: SecurityGroupPeer{CIDR: "10.0.0.0/33"}},
		"missing group":   {Peer: SecurityGroupPeer{SecurityGroupID: "secgroup-nope"}},
		"unshared group":  {Peer: SecurityGroupPeer{SecurityGroupID: other.Meta.ID}},
		"missing network": {Peer: SecurityGroupPeer{NetworkID: "network-nope"}},
	} {
		if _, err := svc.CreateSecurityGroup(ctx, "tenant-a", "x-"+name, SecurityGroupSpec{IngressRules: []SecurityGroupRule{r}}, resource.Metadata{}); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: got %v, want ErrValidation", name, err)
		}
	}
	ok := SecurityGroupSpec{IngressRules: []SecurityGroupRule{
		{Protocol: "tcp", PortRange: "22", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}},
		{Protocol: "tcp", Peer: SecurityGroupPeer{CIDR: "::/0"}},
		{Peer: SecurityGroupPeer{SecurityGroupID: SelfSecurityGroup}},
		{Protocol: "icmp", Peer: SecurityGroupPeer{NetworkID: netA}},
	}}
	if _, err := svc.CreateSecurityGroup(ctx, "tenant-a", "ok", ok, resource.Metadata{}); err != nil {
		t.Fatalf("valid group: %v", err)
	}
	// Sharing makes b's group usable by a: as a peer, and to attach.
	other.Spec.SharedWithTenantIDs = []string{"tenant-a"}
	if _, err := svc.UpdateSecurityGroup(ctx, "tenant-b", other); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSecurityGroup(ctx, "tenant-a", "peer-b", SecurityGroupSpec{IngressRules: []SecurityGroupRule{{Peer: SecurityGroupPeer{SecurityGroupID: other.Meta.ID}}}}, resource.Metadata{}); err != nil {
		t.Fatalf("shared group as peer: %v", err)
	}
	if _, err := svc.GetSecurityGroup(ctx, "tenant-a", other.Meta.ID); err != nil {
		t.Fatalf("Get of a group shared with the caller: %v", err)
	}
	if _, err := svc.GetSecurityGroup(ctx, "tenant-c", other.Meta.ID); !errors.Is(err, ErrSecurityGroupNotFound) {
		t.Fatalf("Get by an unrelated tenant: %v", err)
	}
	// Referenced by a's group now: can't be deleted.
	if err := svc.DeleteSecurityGroup(ctx, "tenant-b", other.Meta.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete referenced group: %v", err)
	}
}

func TestSecurityGroup_AttachAndPolicy(t *testing.T) {
	svc, ctx := newTestService(t)
	netID := testNetwork(t, ctx, svc, "tenant-a")
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.0/24"}}})
	web, err := svc.CreateSecurityGroup(ctx, "tenant-a", "web", SecurityGroupSpec{
		IngressRules: []SecurityGroupRule{{Protocol: "tcp", PortRange: "80", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}}, {Peer: SecurityGroupPeer{SecurityGroupID: SelfSecurityGroup}}},
		EgressRules:  []SecurityGroupRule{{Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}}},
	}, resource.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	n1 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "n1", NetworkInterfaceSpec{VMID: "vm1", SubnetID: sn.Meta.ID, SecurityGroupIDs: []string{web.Meta.ID}}, sn)
	n2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "n2", NetworkInterfaceSpec{VMID: "vm2", SubnetID: sn.Meta.ID, SecurityGroupIDs: []string{web.Meta.ID}}, sn)
	n3 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "n3", NetworkInterfaceSpec{VMID: "vm3", SubnetID: sn.Meta.ID}, sn)
	// Only interfaces of Running VMs (status.hypervisor set) are members.
	for _, n := range []*NetworkInterface{n1, n2, n3} {
		svc.syncInterfaceHypervisor(ctx, "tenant-a", n.Spec.VMID, "hv-1")
	}

	p, err := svc.SecurityPolicy(ctx, *n1)
	if err != nil {
		t.Fatal(err)
	}
	set := setNameSG(web.Meta.ID)
	if !slices.Contains(p.IngressRules, PolicyRule{Set: set}) || !slices.Contains(p.IngressRules, PolicyRule{Protocol: "tcp", PortRange: "80", CIDR: "0.0.0.0/0"}) {
		t.Fatalf("ingress = %+v", p.IngressRules)
	}
	if len(p.Sets) != 1 || p.Sets[0].Name != set || p.Sets[0].Version == 0 || len(p.Sets[0].Members) != 2 ||
		!slices.Contains(p.Sets[0].Members, n1.Status.IPAddress) || !slices.Contains(p.Sets[0].Members, n2.Status.IPAddress) {
		t.Fatalf("sets = %+v, want %s with n1/n2's addresses", p.Sets, set)
	}

	// The attached groups change only through SetSecurityGroups.
	if svc.syncInterfaceHypervisor(ctx, "tenant-a", "vm2", ""); true {
		if p, _ := svc.SecurityPolicy(ctx, *n1); len(p.Sets[0].Members) != 1 {
			t.Fatalf("a stopped VM's address should leave the set: %v", p.Sets[0].Members)
		}
		svc.syncInterfaceHypervisor(ctx, "tenant-a", "vm2", "hv-1")
	}
	cur, _ := svc.GetNetworkInterface(ctx, "tenant-a", n3.Meta.ID)
	cur.Spec.SecurityGroupIDs = []string{web.Meta.ID}
	if _, err := svc.UpdateNetworkInterface(ctx, cur); !errors.Is(err, ErrValidation) {
		t.Fatalf("Update changing groups: %v", err)
	}
	if _, err := svc.SetSecurityGroups(ctx, "tenant-a", n3.Meta.ID, []string{web.Meta.ID, web.Meta.ID}); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicate group: %v", err)
	}
	if _, err := svc.SetSecurityGroups(ctx, "tenant-a", n3.Meta.ID, []string{web.Meta.ID}); err != nil {
		t.Fatal(err)
	}
	if p, _ = svc.SecurityPolicy(ctx, *n1); len(p.Sets[0].Members) != 3 {
		t.Fatalf("after attaching n3: members = %v", p.Sets[0].Members)
	}
	if _, err := svc.SetSecurityGroups(ctx, "tenant-a", n3.Meta.ID, nil); err != nil {
		t.Fatal(err)
	}
	n3now, _ := svc.GetNetworkInterface(ctx, "tenant-a", n3.Meta.ID)
	if p, _ = svc.SecurityPolicy(ctx, *n3now); len(p.IngressRules)+len(p.EgressRules) != 0 {
		t.Fatalf("no groups should mean no rules (deny all), got %+v", p)
	}
	if err := svc.DeleteSecurityGroup(ctx, "tenant-a", web.Meta.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete attached group: %v", err)
	}
	// Another tenant can't attach an unshared group.
	if err := svc.validateAttachSecurityGroups(ctx, "tenant-b", []string{web.Meta.ID}); !errors.Is(err, ErrValidation) {
		t.Fatalf("attach by another tenant: %v", err)
	}
}
