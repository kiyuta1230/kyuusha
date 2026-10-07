package network

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// next returns the next message on sub, failing after a short wait.
func next(t *testing.T, sub *PolicySubscription) PolicyMessage {
	t.Helper()
	select {
	case m := <-sub.Messages():
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no policy message")
		return PolicyMessage{}
	}
}

// waitFor drains sub until pred matches a message.
func waitFor(t *testing.T, sub *PolicySubscription, what string, pred func(PolicyMessage) bool) PolicyMessage {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-sub.Messages():
			if pred(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("no policy message with %s", what)
			return PolicyMessage{}
		}
	}
}

func setUpdate(m PolicyMessage, name string) (SetUpdate, bool) {
	for _, u := range m.Sets {
		if u.Name == name {
			return u, true
		}
	}
	return SetUpdate{}, false
}

func runningNIC(t *testing.T, ctx context.Context, svc *Service, sn *Subnet, name, vm, hv string, groups ...string) *NetworkInterface {
	t.Helper()
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, sn.Meta.TenantID, name, NetworkInterfaceSpec{VMID: vm, SubnetID: sn.Meta.ID, SecurityGroupIDs: groups}, sn)
	svc.syncInterfaceHypervisor(ctx, sn.Meta.TenantID, vm, hv)
	out, err := svc.GetNetworkInterface(ctx, sn.Meta.TenantID, n.Meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPolicyHub drives the xDS-like distribution end to end against etcd:
// a subscription gets full state, a member joining or leaving a set sends
// deltas only to streams holding that set, a group's rule change and a
// SetSecurityGroups resend the affected interface's policy, and a
// periodic resend sends everything again.
func TestPolicyHub(t *testing.T) {
	svc, ctx := newTestService(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	netID := testNetwork(t, ctx, svc, "tenant-a")
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1"}}})
	n1 := runningNIC(t, ctx, svc, sn, "n1", "vm-1", "hv-1")
	netSet := setNameNetwork(netID)

	hub := NewPolicyHub(svc)
	go hub.Run(ctx)
	s1 := hub.Connect("hv-1")
	defer s1.Close()
	s1.Subscribe([]string{n1.Meta.ID})
	s2 := hub.Connect("hv-2")
	defer s2.Close()
	s2.Subscribe(nil)

	m := waitFor(t, s1, "n1's policy", func(m PolicyMessage) bool { return len(m.Interfaces) == 1 })
	p := m.Interfaces[0]
	if p.IfaceID != n1.Meta.ID || p.VMID != "vm-1" || p.IPAddress != n1.Status.IPAddress || p.MACAddress == "" ||
		p.SubnetCIDR != "10.0.0.0/24" || p.GatewayIP != "10.0.0.1" || p.Attach.NetworkClass != "test-user" ||
		!slices.Contains(p.Policy.IngressRules, PolicyRule{Set: netSet}) || p.Version == 0 {
		t.Fatalf("n1 policy = %+v", p)
	}
	if u, ok := setUpdate(m, netSet); !ok || !u.Full || !slices.Equal(u.Members, []string{n1.Status.IPAddress}) {
		t.Fatalf("full copy of %s = %+v (sets %+v)", netSet, u, m.Sets)
	}

	// n2 comes up on hv-2 (not subscribed there yet): hv-1, which holds the
	// Network's set, gets the delta; hv-2 holds no sets and gets nothing.
	n2 := runningNIC(t, ctx, svc, sn, "n2", "vm-2", "hv-2")
	m = waitFor(t, s1, "n2 joining", func(m PolicyMessage) bool {
		u, ok := setUpdate(m, netSet)
		return ok && slices.Contains(u.Add, n2.Status.IPAddress)
	})
	if u, _ := setUpdate(m, netSet); u.Full || u.Version <= p.Version {
		t.Fatalf("delta = %+v, want a non-full update newer than %d", u, p.Version)
	}
	// hv-2 subscribes to n2: its policy plus a full copy holding both.
	s2.Subscribe([]string{n2.Meta.ID})
	m = waitFor(t, s2, "n2's policy", func(m PolicyMessage) bool { return len(m.Interfaces) == 1 })
	if u, ok := setUpdate(m, netSet); !ok || !u.Full || len(u.Members) != 2 {
		t.Fatalf("hv-2's full copy = %+v", m.Sets)
	}

	// A group's rule change resends the policy of every subscribed member.
	g, _ := svc.GetSecurityGroup(ctx, "tenant-a", n1.Spec.SecurityGroupIDs[0])
	g.Spec.IngressRules = append(g.Spec.IngressRules, SecurityGroupRule{Protocol: "tcp", PortRange: "22", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}})
	if _, err := svc.UpdateSecurityGroup(ctx, "tenant-a", g); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*PolicySubscription{s1, s2} {
		waitFor(t, sub, "the rule change", func(m PolicyMessage) bool {
			return len(m.Interfaces) == 1 && slices.Contains(m.Interfaces[0].Policy.IngressRules, PolicyRule{Protocol: "tcp", PortRange: "22", CIDR: "0.0.0.0/0"})
		})
	}

	// n2 switches to a "peers" group (in: sg=self): hv-2 gets n2's new
	// policy with a full copy of the new set. (n2 stays in the Network's
	// set, so hv-1 sees no change.)
	peers, err := svc.CreateSecurityGroup(ctx, "tenant-a", "peers", SecurityGroupSpec{IngressRules: []SecurityGroupRule{{Peer: SecurityGroupPeer{SecurityGroupID: SelfSecurityGroup}}}}, resource.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSecurityGroups(ctx, "tenant-a", n2.Meta.ID, []string{peers.Meta.ID}); err != nil {
		t.Fatal(err)
	}
	peerSet := setNameSG(peers.Meta.ID)
	m = waitFor(t, s2, "n2's new groups", func(m PolicyMessage) bool { return len(m.Interfaces) == 1 })
	if u, ok := setUpdate(m, peerSet); !ok || !u.Full || !slices.Equal(u.Members, []string{n2.Status.IPAddress}) {
		t.Fatalf("hv-2 after SetSecurityGroups: sets %+v", m.Sets)
	}

	// n2's VM stops: it leaves both sets; hv-1 (holding the Network's
	// set) and hv-2 (holding sg:peers) each get their removal.
	svc.syncInterfaceHypervisor(ctx, "tenant-a", "vm-2", "")
	waitFor(t, s1, "n2 leaving the Network set", func(m PolicyMessage) bool {
		u, ok := setUpdate(m, netSet)
		return ok && slices.Contains(u.Remove, n2.Status.IPAddress)
	})
	waitFor(t, s2, "n2 leaving sg:peers", func(m PolicyMessage) bool {
		u, ok := setUpdate(m, peerSet)
		return ok && slices.Contains(u.Remove, n2.Status.IPAddress)
	})

	// An Ack is just bookkeeping; it must not disturb anything.
	s1.Ack(m.Revision, "")
	s1.Ack(m.Revision, "boom")
}

func TestPolicyHub_PeriodicResend(t *testing.T) {
	old := policyResendInterval
	policyResendInterval = 200 * time.Millisecond
	defer func() { policyResendInterval = old }()
	svc, ctx := newTestService(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	netID := testNetwork(t, ctx, svc, "tenant-a")
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "s", SubnetSpec{NetworkID: netID, Zone: "z", RequestedAddresses: []SubnetAddress{{CIDR: "10.0.0.0/24"}}})
	n1 := runningNIC(t, ctx, svc, sn, "n1", "vm-1", "hv-1")
	hub := NewPolicyHub(svc)
	go hub.Run(ctx)
	s := hub.Connect("hv-1")
	defer s.Close()
	s.Subscribe([]string{n1.Meta.ID})
	next(t, s)
	m := next(t, s)
	if len(m.Interfaces) != 1 {
		t.Fatalf("periodic resend = %+v", m)
	}
	if u, ok := setUpdate(m, setNameNetwork(netID)); !ok || !u.Full {
		t.Fatalf("periodic resend sets = %+v", m.Sets)
	}
}
