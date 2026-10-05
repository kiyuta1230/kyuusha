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
		TapName:   tap,
		GatewayIP: "10.123.45.1",
		IngressRules: []Rule{
			{Protocol: "tcp", PortRange: "22", CIDR: "0.0.0.0/0"},
		},
		EgressRules: []Rule{
			{Protocol: "tcp", PortRange: "443", CIDR: "0.0.0.0/0"},
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

	// Re-Apply (e.g. update_acl changing the rule set) must not
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

// TestSetRules covers a rule naming an address set: the set exists in
// both tables, the rule matches @set, UpdateSets deltas/full copies change
// its members, and Remove of the last referencing tap deletes it.
func TestSetRules(t *testing.T) {
	const tap = "nftacltest1"
	t.Cleanup(func() { Remove(tap) })

	iface := Interface{
		TapName:      tap,
		IngressRules: []Rule{{Set: "sg:sg-1"}},
		EgressRules:  []Rule{{CIDR: "0.0.0.0/0"}, {CIDR: "::/0", Protocol: "tcp"}},
		Sets:         []SetUpdate{{Name: "sg:sg-1", Full: true, Members: []string{"10.125.0.5"}}},
	}
	if err := Apply(iface); err != nil {
		t.Skipf("skipping: nftables manipulation needs CAP_NET_ADMIN: %v", err)
	}
	set := setName("sg:sg-1")
	ruleset := mustListRuleset(t)
	if strings.Count(ruleset, "ip saddr @"+set) != 2 {
		t.Fatalf("expected the set rule in the -out chain of both tables, got:\n%s", ruleset)
	}
	members := func() []string {
		m, ok, err := setElements(set)
		if err != nil || !ok {
			t.Fatalf("setElements: ok=%v err=%v", ok, err)
		}
		return m
	}
	if got := members(); len(got) != 1 || got[0] != "10.125.0.5" {
		t.Fatalf("members = %v", got)
	}
	if err := UpdateSets([]SetUpdate{{Name: "sg:sg-1", Add: []string{"10.125.0.6"}, Remove: []string{"10.125.0.5"}}}); err != nil {
		t.Fatal(err)
	}
	if got := members(); len(got) != 1 || got[0] != "10.125.0.6" {
		t.Fatalf("after delta: members = %v", got)
	}
	if err := UpdateSets([]SetUpdate{{Name: "sg:sg-1", Full: true, Members: []string{"10.125.0.7", "10.125.0.8"}}, {Name: "sg:not-here", Add: []string{"10.0.0.1"}}}); err != nil {
		t.Fatal(err)
	}
	if got := members(); len(got) != 2 {
		t.Fatalf("after full copy: members = %v", got)
	}
	if err := Remove(tap); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := setElements(set); ok {
		t.Fatal("set still exists after its last referencing tap was removed")
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
