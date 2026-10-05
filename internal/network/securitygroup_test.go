package network

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// startTestNATS mirrors internal/compute/liveops_test.go's identically-named
// helper -- duplicated per-package rather than shared, same reasoning (see
// that copy's doc comment: avoiding an import cycle risk, and each package
// owning its own EnsureStreams).
func startTestNATS(t *testing.T) jetstream.JetStream {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("start embedded nats: %v", err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats server never became ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("connect to embedded nats: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := EnsureStreams(context.Background(), js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	return js
}

// subscribeCommands collects every T published on subject.
func subscribeCommands[T any](t *testing.T, ctx context.Context, js jetstream.JetStream, durable, subject string) func() []T {
	t.Helper()
	stream, err := js.Stream(ctx, cmdStreamName)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{Durable: durable, FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	ch := make(chan T, 100)
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var cmd T
		if json.Unmarshal(msg.Data(), &cmd) == nil {
			ch <- cmd
		}
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	t.Cleanup(consumeCtx.Stop)
	var got []T
	return func() []T {
		deadline := time.After(2 * time.Second)
		for {
			select {
			case c := <-ch:
				got = append(got, c)
			case <-time.After(200 * time.Millisecond):
				return got
			case <-deadline:
				return got
			}
		}
	}
}

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

func TestSetSecurityGroupsPublishesUpdateACL(t *testing.T) {
	ctx := context.Background()
	js := startTestNATS(t)
	computeClient := &FakeVirtualMachineClient{Existing: map[string]bool{"vm-1": true}, Hypervisor: map[string]string{"vm-1": "hypervisor-1"}}
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, computeClient, js)
	if err != nil {
		t.Fatal(err)
	}
	sn, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "s", userSubnet(t, ctx, svc, "tenant-a", "z", "10.0.1.0/24", "10.0.1.1"),
		resource.Metadata{Labels: map[string]string{"vpc.example.com/id": "vpc-1"}})
	if err != nil {
		t.Fatal(err)
	}
	svc.tryAllocateSubnet(ctx, sn)
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	svc.syncInterfaceHypervisor(ctx, "tenant-a", "vm-1", "hypervisor-1")
	received := subscribeCommands[UpdateACLCommand](t, ctx, js, "acl", CmdSubjectUpdateACL("hypervisor-1"))

	ssh, err := svc.CreateSecurityGroup(ctx, "tenant-a", "ssh", SecurityGroupSpec{IngressRules: []SecurityGroupRule{{Protocol: "tcp", PortRange: "22", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}}}}, resource.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSecurityGroups(ctx, "tenant-a", n.Meta.ID, append(n.Spec.SecurityGroupIDs, ssh.Meta.ID)); err != nil {
		t.Fatal(err)
	}
	cmds := received()
	if len(cmds) != 1 {
		t.Fatalf("got %d update_acl commands, want 1", len(cmds))
	}
	cmd := cmds[0]
	if cmd.IfaceID != n.Meta.ID || cmd.SubnetCIDR != "10.0.1.0/24" || cmd.GatewayIP != "10.0.1.1" || cmd.SubnetLabels["vpc.example.com/id"] != "vpc-1" ||
		cmd.IPAddress != n.Status.IPAddress || cmd.MACAddress != n.Status.MACAddress || cmd.Attach.NetworkClass != "test-user" {
		t.Fatalf("update_acl = %+v", cmd)
	}
	if len(cmd.Policy.SecurityGroupIDs) != 2 ||
		!slices.Contains(cmd.Policy.IngressRules, PolicyRuleInfo{Protocol: "tcp", PortRange: "22", CIDR: "0.0.0.0/0"}) ||
		len(cmd.Policy.Sets) != 1 || cmd.Policy.Sets[0].Name != setNameNetwork(sn.Spec.NetworkID) || !slices.Contains(cmd.Policy.Sets[0].Members, n.Status.IPAddress) {
		t.Fatalf("policy = %+v", cmd.Policy)
	}
}

// TestSGSync drives the reconciler-side distribution directly: a member
// joining sends a delta to every host referencing the set, an interface
// arriving on a host gets full copies, the periodic sync sends full copies,
// and a group's rule change re-sends update_acl.
func TestSGSync(t *testing.T) {
	ctx := context.Background()
	js := startTestNATS(t)
	computeClient := &FakeVirtualMachineClient{Existing: map[string]bool{"vm-1": true, "vm-2": true}, Hypervisor: map[string]string{"vm-1": "hv-1", "vm-2": "hv-2"}}
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, computeClient, js)
	if err != nil {
		t.Fatal(err)
	}
	netID := testNetwork(t, ctx, svc, "tenant-a")
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.0/24"}}})
	sets1 := subscribeCommands[UpdateSetsCommand](t, ctx, js, "sets1", CmdSubjectUpdateSets("hv-1"))
	sets2 := subscribeCommands[UpdateSetsCommand](t, ctx, js, "sets2", CmdSubjectUpdateSets("hv-2"))
	acl1 := subscribeCommands[UpdateACLCommand](t, ctx, js, "acl1", CmdSubjectUpdateACL("hv-1"))

	y := newSGSyncer(svc)
	feedGroups := func() {
		gs, _ := svc.secgroups.List(ctx, "")
		for _, g := range gs {
			y.handleGroup(ctx, SecurityGroupEvent{Type: EventAdded, Object: g, ResourceVersion: g.Meta.ResourceVersion})
		}
	}
	feedGroups()
	setNIC := func(n *NetworkInterface, hv string) NetworkInterface {
		cur, _ := svc.GetNetworkInterface(ctx, n.Meta.TenantID, n.Meta.ID)
		cur.Status.Hypervisor = hv
		out, err := svc.interfaces.Update(ctx, *cur)
		if err != nil {
			t.Fatal(err)
		}
		y.handleNIC(ctx, NetworkInterfaceEvent{Type: EventModified, Object: out, ResourceVersion: out.Meta.ResourceVersion})
		return out
	}

	n1 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "n1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	setNIC(n1, "hv-1")
	set := setNameNetwork(netID)
	got := sets1()
	if len(got) != 1 || !got[0].Sets[0].Full || got[0].Sets[0].Name != set || !slices.Equal(got[0].Sets[0].Members, []string{n1.Status.IPAddress}) {
		t.Fatalf("arrival on hv-1: %+v", got)
	}
	// n2 joins the Network: hv-1 (whose n1 references network:<id> through
	// the default group) gets a delta adding it.
	n2 := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "n2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)
	n2now := setNIC(n2, "hv-2")
	got = sets1()[1:]
	if len(got) != 1 || got[0].Sets[0].Full || !slices.Equal(got[0].Sets[0].Add, []string{n2.Status.IPAddress}) || got[0].ObservedAt.IsZero() {
		t.Fatalf("delta on hv-1 for n2 joining: %+v", got)
	}
	if got := sets2(); len(got) != 1 || len(got[0].Sets[0].Members) != 2 {
		t.Fatalf("arrival on hv-2: %+v", got)
	}
	// n2 leaves (deleted): hv-1 gets a removal.
	y.handleNIC(ctx, NetworkInterfaceEvent{Type: EventDeleted, Object: n2now, ResourceVersion: n2now.Meta.ResourceVersion + 1})
	got = sets1()[2:]
	if len(got) != 1 || !slices.Equal(got[0].Sets[0].Remove, []string{n2.Status.IPAddress}) {
		t.Fatalf("delta on hv-1 for n2 leaving: %+v", got)
	}
	// Periodic full sync: hv-1 gets a full copy read from etcd (n2 still
	// exists there, since only the event was simulated).
	y.fullSync(ctx)
	got = sets1()[3:]
	if len(got) != 1 || !got[0].Sets[0].Full || len(got[0].Sets[0].Members) != 2 {
		t.Fatalf("full sync on hv-1: %+v", got)
	}
	// A rule change in the default group re-sends update_acl to hv-1's n1.
	g, _ := svc.GetSecurityGroup(ctx, "tenant-a", n1.Spec.SecurityGroupIDs[0])
	g.Spec.IngressRules = append(g.Spec.IngressRules, SecurityGroupRule{Protocol: "tcp", PortRange: "22", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}})
	updated, err := svc.UpdateSecurityGroup(ctx, "tenant-a", g)
	if err != nil {
		t.Fatal(err)
	}
	y.handleGroup(ctx, SecurityGroupEvent{Type: EventModified, Object: *updated, ResourceVersion: updated.Meta.ResourceVersion})
	if acls := acl1(); len(acls) != 1 || acls[0].IfaceID != n1.Meta.ID || acls[0].ResourceVersion < updated.Meta.ResourceVersion {
		t.Fatalf("update_acl after a rule change: %+v", acls)
	}
}
