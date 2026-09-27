package nftacl

import (
	"os/exec"
	"strings"
	"testing"
)

// TestApplyAndRemove exercises the real nftables path, same
// skip-if-unprivileged convention as internal/compute-agent/netsetup's own
// TestWireCreatesTapAndBridge: manipulating nftables state needs
// CAP_NET_ADMIN, not available to an ordinary unprivileged `go test` run.
// Unlike netsetup's tap/bridge, the tap name itself doesn't need to be a
// real device here -- see the package doc comment: iifname/oifname
// matching in the jumped-to chains doesn't require the interface to exist
// at rule-add time, only at actual packet-forward time (which this test,
// deliberately not a full network-namespace integration test, doesn't
// exercise -- see the playground verification plan for that).
func TestApplyAndRemove(t *testing.T) {
	const tap = "nftacltest0"
	t.Cleanup(func() { Remove(tap) })

	iface := Interface{
		TapName:    tap,
		SubnetCIDR: "10.123.45.0/24",
		GatewayIP:  "10.123.45.1",
		IngressRules: []FirewallRule{
			{Protocol: "tcp", PortRange: "22", SourceCIDR: "0.0.0.0/0", Action: "allow"},
		},
		EgressRules: []FirewallRule{
			{Protocol: "tcp", PortRange: "443", SourceCIDR: "0.0.0.0/0", Action: "allow"},
		},
	}
	if err := Apply(iface); err != nil {
		t.Skipf("skipping: nftables manipulation needs CAP_NET_ADMIN: %v", err)
	}

	ruleset := mustListRuleset(t)
	if !strings.Contains(ruleset, inChain(tap)) || !strings.Contains(ruleset, outChain(tap)) {
		t.Fatalf("ruleset missing one of %s/%s chains:\n%s", inChain(tap), outChain(tap), ruleset)
	}
	if !strings.Contains(ruleset, "jump "+inChain(tap)) || !strings.Contains(ruleset, "jump "+outChain(tap)) {
		t.Fatalf("base chain missing a jump rule into %s/%s:\n%s", inChain(tap), outChain(tap), ruleset)
	}
	if !strings.Contains(ruleset, "ct state established,related return") {
		t.Fatalf("ruleset missing the established/related baseline:\n%s", ruleset)
	}
	if !strings.Contains(ruleset, "tcp dport 22") {
		t.Fatalf("ruleset missing the ingress rule's tcp dport 22 match:\n%s", ruleset)
	}
	if !strings.Contains(ruleset, "tcp dport 443") {
		t.Fatalf("ruleset missing the egress rule's tcp dport 443 match:\n%s", ruleset)
	}

	// Re-Apply (e.g. UpdateFirewallRules changing the rule set) must not
	// duplicate rules -- flush-then-reload the tap's own two chains, and
	// must not add a second, redundant pair of jump rules into the shared
	// base chain.
	iface.IngressRules = nil
	if err := Apply(iface); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	ruleset = mustListRuleset(t)
	if strings.Contains(ruleset, "tcp dport 22") {
		t.Fatalf("ruleset still contains the removed ingress rule after re-Apply:\n%s", ruleset)
	}
	if !strings.Contains(ruleset, "tcp dport 443") {
		t.Fatalf("re-Apply lost the still-present egress rule:\n%s", ruleset)
	}
	if strings.Count(ruleset, "jump "+inChain(tap)) != 1 {
		t.Fatalf("expected exactly one jump rule into %s after re-Apply, ruleset:\n%s", inChain(tap), ruleset)
	}

	if err := Remove(tap); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ruleset = mustListRuleset(t)
	if strings.Contains(ruleset, inChain(tap)) || strings.Contains(ruleset, outChain(tap)) {
		t.Fatalf("ruleset still contains %s/%s after Remove:\n%s", inChain(tap), outChain(tap), ruleset)
	}

	// Remove must be safe to call again once nothing is left to remove.
	if err := Remove(tap); err != nil {
		t.Fatalf("second Remove (nothing left): %v", err)
	}
}

// TestApplyAnyProtocolRule covers the empty-Protocol ("any protocol")
// rule shape Service.EffectiveFirewallRules emits for mesh_group-derived
// synthetic rules -- writeRule silently dropped this case entirely until
// a real playground mesh_group test caught it (a unit test alone hadn't).
func TestApplyAnyProtocolRule(t *testing.T) {
	const tap = "nftacltest1"
	t.Cleanup(func() { Remove(tap) })

	iface := Interface{
		TapName:      tap,
		SubnetCIDR:   "10.124.0.0/24",
		IngressRules: []FirewallRule{{SourceCIDR: "10.125.0.0/24", Action: "allow"}},
		EgressRules:  []FirewallRule{{SourceCIDR: "10.125.0.0/24", Action: "allow"}},
	}
	if err := Apply(iface); err != nil {
		t.Skipf("skipping: nftables manipulation needs CAP_NET_ADMIN: %v", err)
	}

	ruleset := mustListRuleset(t)
	if strings.Count(ruleset, "10.125.0.0/24") != 2 {
		t.Fatalf("expected the any-protocol rule in both %s/%s chains, got:\n%s", inChain(tap), outChain(tap), ruleset)
	}
}

func mustListRuleset(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("nft", "list", "ruleset").CombinedOutput()
	if err != nil {
		t.Fatalf("nft list ruleset: %v: %s", err, out)
	}
	return string(out)
}
