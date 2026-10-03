package network

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

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

func TestService_UpdateFirewallRulesReplacesBothLists(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	subnet := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenant, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnet.Meta.ID}, subnet)

	ingress := []FirewallRule{{Protocol: "tcp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "allow"}}
	egress := []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "0.0.0.0/0", Action: "allow"}}

	updated, err := svc.UpdateFirewallRules(ctx, tenant, n.Meta.ID, ingress, egress)
	if err != nil {
		t.Fatalf("UpdateFirewallRules: %v", err)
	}
	if !firewallRulesEqual(updated.Spec.IngressRules, ingress) {
		t.Fatalf("IngressRules = %+v, want %+v", updated.Spec.IngressRules, ingress)
	}
	if !firewallRulesEqual(updated.Spec.EgressRules, egress) {
		t.Fatalf("EgressRules = %+v, want %+v", updated.Spec.EgressRules, egress)
	}

	// Re-Get to confirm it actually persisted, not just the returned value.
	got, err := svc.GetNetworkInterface(ctx, tenant, n.Meta.ID)
	if err != nil {
		t.Fatalf("GetNetworkInterface: %v", err)
	}
	if !firewallRulesEqual(got.Spec.IngressRules, ingress) || !firewallRulesEqual(got.Spec.EgressRules, egress) {
		t.Fatalf("stored spec did not reflect UpdateFirewallRules: %+v", got.Spec)
	}
}

func TestService_UpdateFirewallRulesRejectsInvalidRule(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	subnet := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenant, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnet.Meta.ID}, subnet)

	cases := []FirewallRule{
		{Protocol: "sctp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "allow"},
		{Protocol: "tcp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "permit"},
		{Protocol: "tcp", PortRange: "22", SourceCIDR: "not-a-cidr", Action: "allow"},
		{Protocol: "tcp", PortRange: "not-a-port", SourceCIDR: "0.0.0.0/0", Action: "allow"},
		{Protocol: "tcp", PortRange: "100-50", SourceCIDR: "0.0.0.0/0", Action: "allow"},
	}
	for _, bad := range cases {
		if _, err := svc.UpdateFirewallRules(ctx, tenant, n.Meta.ID, []FirewallRule{bad}, nil); !errors.Is(err, ErrValidation) {
			t.Errorf("UpdateFirewallRules(%+v): got %v, want ErrValidation", bad, err)
		}
	}
}

func TestService_UpdateNetworkInterfaceRejectsFirewallRuleChange(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	subnet := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenant, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnet.Meta.ID}, subnet)

	mutated := *n
	mutated.Spec.IngressRules = []FirewallRule{{Protocol: "tcp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "allow"}}
	if _, err := svc.UpdateNetworkInterface(ctx, &mutated); !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateNetworkInterface changing ingress_rules: got %v, want ErrValidation", err)
	}

	mutated = *n
	mutated.Spec.EgressRules = []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "0.0.0.0/0", Action: "allow"}}
	if _, err := svc.UpdateNetworkInterface(ctx, &mutated); !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateNetworkInterface changing egress_rules: got %v, want ErrValidation", err)
	}

	// Sanity: an Update that leaves both lists untouched must still work.
	unchanged := *n
	if _, err := svc.UpdateNetworkInterface(ctx, &unchanged); err != nil {
		t.Fatalf("UpdateNetworkInterface with unchanged rules: %v", err)
	}
}

// subscribeUpdateACLCommands mirrors internal/compute/migrate_test.go's
// subscribeDeleteCommands, for CmdSubjectUpdateACL instead.
func subscribeUpdateACLCommands(t *testing.T, ctx context.Context, js jetstream.JetStream, hypervisor string) *[]UpdateACLCommand {
	t.Helper()
	var received []UpdateACLCommand
	stream, err := js.Stream(ctx, cmdStreamName)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "test-update-acl-watcher-" + hypervisor,
		FilterSubject: CmdSubjectUpdateACL(hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var cmd UpdateACLCommand
		if err := json.Unmarshal(msg.Data(), &cmd); err == nil {
			received = append(received, cmd)
		}
	})
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	t.Cleanup(consumeCtx.Stop)
	return &received
}

func TestService_UpdateFirewallRulesPublishesUpdateACLWhenScheduled(t *testing.T) {
	ctx := context.Background()
	js := startTestNATS(t)
	computeClient := &FakeVirtualMachineClient{
		Existing:   map[string]bool{"vm-1": true},
		Hypervisor: map[string]string{"vm-1": "hypervisor-1"},
	}
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, computeClient, js)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	subnet := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24", GatewayIP: "10.0.1.1"})
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenant, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnet.Meta.ID}, subnet)

	received := subscribeUpdateACLCommands(t, ctx, js, "hypervisor-1")

	ingress := []FirewallRule{{Protocol: "tcp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "allow"}}
	if _, err := svc.UpdateFirewallRules(ctx, tenant, n.Meta.ID, ingress, nil); err != nil {
		t.Fatalf("UpdateFirewallRules: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for len(*received) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for update_acl command to be published")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cmd := (*received)[0]
	if cmd.IfaceID != n.Meta.ID || cmd.VMID != "vm-1" || cmd.TenantID != tenant {
		t.Fatalf("UpdateACLCommand = %+v, want iface_id=%s vm_id=vm-1 tenant_id=%s", cmd, n.Meta.ID, tenant)
	}
	if cmd.SubnetCIDR != "10.0.1.0/24" || cmd.GatewayIP != "10.0.1.1" || cmd.SubnetID != subnet.Meta.ID {
		t.Fatalf("UpdateACLCommand subnet info = %+v, want subnet_id=%s cidr=10.0.1.0/24 gateway=10.0.1.1", cmd, subnet.Meta.ID)
	}
	if cmd.IPAddress == "" || cmd.IPAddress != n.Status.IPAddress || cmd.MACAddress == "" || cmd.MACAddress != n.Status.MACAddress {
		t.Fatalf("UpdateACLCommand address = ip %q mac %q, want the interface's own ip %q mac %q", cmd.IPAddress, cmd.MACAddress, n.Status.IPAddress, n.Status.MACAddress)
	}
	if len(cmd.IngressRules) != 1 || cmd.IngressRules[0].PortRange != "22" {
		t.Fatalf("UpdateACLCommand.IngressRules = %+v, want one rule with port_range=22", cmd.IngressRules)
	}
}

func TestService_UpdateFirewallRulesDoesNotPublishWhenUnscheduled(t *testing.T) {
	ctx := context.Background()
	js := startTestNATS(t)
	// vm-1 exists but has no Hypervisor set -- not yet scheduled.
	computeClient := &FakeVirtualMachineClient{Existing: map[string]bool{"vm-1": true}}
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, computeClient, js)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	subnet := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, tenant, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnet.Meta.ID}, subnet)

	// No hypervisor is known, so there's no meaningful subject to watch --
	// this test instead just confirms UpdateFirewallRules doesn't error or
	// hang when publishUpdateACL has nothing useful to do.
	if _, err := svc.UpdateFirewallRules(ctx, tenant, n.Meta.ID, nil, nil); err != nil {
		t.Fatalf("UpdateFirewallRules: %v", err)
	}
}
