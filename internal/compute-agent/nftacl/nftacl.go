// Package nftacl is snap's default, built-in ACL enforcement backend
// (used whenever compute-agent's -security-backend-bin is empty -- see
// internal/compute-agent/snap's own doc comment for why ACL enforcement
// is a separate pluggable concern from netsetup's VNAP tap-wiring). It
// shells out to the `nft` CLI (exec.Command, matching netsetup.runIP's own
// exec-heavy style -- no separate Go nftables library dependency), same as
// every other host-network-state mutation in this codebase.
//
// # Why the bridge family, not netdev
//
// A per-tap netdev-family hook (type filter hook ingress/egress device
// <tap>) was the original design, but was abandoned after hands-on testing
// found `ct state established,related` -- needed so return traffic for an
// already-allowed connection isn't independently re-evaluated and blocked
// -- fails outright in the netdev family on a real kernel/nftables build
// ("Protocol error"): netdev hooks sit before the point in the stack where
// conntrack state is established, so this isn't a portability quirk to
// work around, it's architectural. The bridge family's "forward" hook (the
// same hook classic bridge-netfilter iptables rules use) does support
// ct state reliably, at the cost of requiring the tap to actually be a
// port of a Linux bridge -- i.e. this default implementation assumes
// netsetup's own built-in bridge wiring. A non-bridge VNAP wiring plugin
// (e.g. the FRR Type-5 example, which deliberately has no shared bridge --
// see docs/specs/network.md「VNAP」) needs its own paired security-backend
// plugin instead of this default one; that's an accepted, deliberate
// scope boundary, not an oversight.
//
// # Structure
//
// One shared base chain (bridge/forward/policy accept, since this hook
// fires for every bridged packet on the host, not just kyuusha's -- it
// must never affect bridge traffic this package doesn't own) holds one
// pair of guard rules per currently-wired tap:
//
//	iifname "<tap>" jump <tap>-in
//	oifname "<tap>" jump <tap>-out
//
// <tap>-in enforces EgressRules (iifname == tap: the VM's own outbound
// traffic being forwarded) and <tap>-out enforces IngressRules (oifname ==
// tap: traffic about to be delivered to the VM) -- each a plain (non-base)
// chain ending in `return` (this direction's checks passed -- but a
// forwarded packet touches two taps, e.g. VM-to-VM on the same bridge, so
// the *other* tap's own jump rule still needs to run; `return` continues
// the base chain rather than terminating packet evaluation early the way
// `accept` would) for each recognized allow, or a terminal `drop` for a
// recognized deny or an unmatched packet (default-deny once inside a
// tap's own chain). Neither jump rule nor its target chain ever issues
// `accept` itself -- the base chain's own `policy accept` is what finally
// allows a packet once every relevant tap's chain has returned instead of
// dropping.
package nftacl

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// FirewallRule mirrors snap.FirewallRule -- its own copy, not an import,
// same "each layer has its own mirror struct" convention as netsetup's own
// types.
type FirewallRule struct {
	Protocol   string // tcp/udp/icmp
	PortRange  string // e.g. "22", "2379-2380"; ignored for icmp
	SourceCIDR string // the peer CIDR: who may reach the VM (IngressRules) or which peers the VM may reach (EgressRules)
	Action     string // allow/deny
}

type Interface struct {
	TapName    string
	SubnetCIDR string
	GatewayIP  string

	IngressRules []FirewallRule
	EgressRules  []FirewallRule
}

const (
	table     = "kyuusha_acl"
	baseChain = "base"
)

func inChain(tap string) string  { return tap + "-in" }  // iifname == tap -- enforces EgressRules
func outChain(tap string) string { return tap + "-out" } // oifname == tap -- enforces IngressRules

// Apply (re-)installs iface's complete ACL state: idempotent, and safe to
// call again with a fresh rule set (flushes each of the tap's two chains
// before re-adding their rules, rather than diffing -- same "always
// reassert full state" convention as this codebase's other reconcile-style
// operations).
func Apply(iface Interface) error {
	var setup strings.Builder
	fmt.Fprintf(&setup, "add table bridge %s\n", table)
	fmt.Fprintf(&setup, "add chain bridge %s %s { type filter hook forward priority 0; policy accept; }\n", table, baseChain)
	fmt.Fprintf(&setup, "add chain bridge %s %s\n", table, inChain(iface.TapName))
	fmt.Fprintf(&setup, "add chain bridge %s %s\n", table, outChain(iface.TapName))
	if err := run(setup.String()); err != nil {
		return err
	}

	var rules strings.Builder
	fmt.Fprintf(&rules, "flush chain bridge %s %s\n", table, inChain(iface.TapName))
	fmt.Fprintf(&rules, "flush chain bridge %s %s\n", table, outChain(iface.TapName))
	writeBaseline(&rules, inChain(iface.TapName), iface.SubnetCIDR, iface.GatewayIP, "saddr")
	writeBaseline(&rules, outChain(iface.TapName), iface.SubnetCIDR, iface.GatewayIP, "daddr")
	for _, r := range iface.EgressRules {
		writeRule(&rules, inChain(iface.TapName), r, "daddr") // VM -> remote peer: peer is the destination
	}
	for _, r := range iface.IngressRules {
		writeRule(&rules, outChain(iface.TapName), r, "saddr") // remote sender -> VM: peer is the source
	}
	fmt.Fprintf(&rules, "add rule bridge %s %s drop\n", table, inChain(iface.TapName))
	fmt.Fprintf(&rules, "add rule bridge %s %s drop\n", table, outChain(iface.TapName))
	if err := run(rules.String()); err != nil {
		return err
	}

	return ensureJumpRules(iface.TapName)
}

// writeBaseline adds the always-allow rules every tap chain gets
// regardless of its own ingress_rules/egress_rules: established/related
// connections, traffic within the interface's own Subnet CIDR, and the
// Subnet's gateway_ip -- the defense-in-depth default-deny-outside-CIDR
// baseline docs/architecture.md's「防御層としてのNetworkInterface ACL」
// describes, now actually enforced. `return`, not `accept` -- see the
// package doc comment for why.
func writeBaseline(b *strings.Builder, chain, subnetCIDR, gatewayIP, addrKeyword string) {
	fmt.Fprintf(b, "add rule bridge %s %s ct state established,related return\n", table, chain)
	if subnetCIDR != "" {
		fmt.Fprintf(b, "add rule bridge %s %s ip %s %s return\n", table, chain, addrKeyword, subnetCIDR)
	}
	if gatewayIP != "" {
		fmt.Fprintf(b, "add rule bridge %s %s ip %s %s return\n", table, chain, addrKeyword, gatewayIP)
	}
}

func writeRule(b *strings.Builder, chain string, r FirewallRule, addrKeyword string) {
	verdict := "return" // allow: this direction checks out, let the other tap's jump (if any) still run
	if r.Action == "deny" {
		verdict = "drop" // deny is terminal: no need to wait on the other side
	}
	// "" means "any protocol" -- not reachable from a tenant-submitted rule
	// (internal/network's validateFirewallRules requires tcp/udp/icmp), but
	// used by mesh_group-derived synthetic rules (see
	// Service.EffectiveFirewallRules), which trust a sibling Subnet's
	// entire CIDR, not just specific protocols/ports.
	var proto string
	switch r.Protocol {
	case "tcp", "udp":
		proto = fmt.Sprintf("%s dport %s", r.Protocol, r.PortRange)
	case "icmp":
		proto = "ip protocol icmp"
	case "":
		proto = ""
	default:
		return // validated upstream (internal/network's validateFirewallRules); defensively skip an unrecognized protocol rather than emit a malformed rule
	}
	switch {
	case r.SourceCIDR != "" && proto != "":
		fmt.Fprintf(b, "add rule bridge %s %s ip %s %s %s %s\n", table, chain, addrKeyword, r.SourceCIDR, proto, verdict)
	case r.SourceCIDR != "":
		fmt.Fprintf(b, "add rule bridge %s %s ip %s %s %s\n", table, chain, addrKeyword, r.SourceCIDR, verdict)
	case proto != "":
		fmt.Fprintf(b, "add rule bridge %s %s %s %s\n", table, chain, proto, verdict)
	default:
		fmt.Fprintf(b, "add rule bridge %s %s %s\n", table, chain, verdict) // any protocol, any address -- a blanket allow/deny
	}
}

// ensureJumpRules adds the base chain's two guard rules for tapName
// (iifname/oifname jump into its two chains) unless they're already
// there -- checked via `nft -j list chain`, since a plain `add rule` isn't
// idempotent the way `add table`/`add chain` are (it would append a
// duplicate jump pair on every call, e.g. every UpdateFirewallRules over a
// VM's lifetime).
func ensureJumpRules(tapName string) error {
	existing, err := jumpTargets(baseChain)
	if err != nil {
		return err
	}
	var add strings.Builder
	if !existing[inChain(tapName)] {
		fmt.Fprintf(&add, "add rule bridge %s %s iifname %q jump %s\n", table, baseChain, tapName, inChain(tapName))
	}
	if !existing[outChain(tapName)] {
		fmt.Fprintf(&add, "add rule bridge %s %s oifname %q jump %s\n", table, baseChain, tapName, outChain(tapName))
	}
	if add.Len() == 0 {
		return nil
	}
	return run(add.String())
}

// nftRuleset is just enough of `nft -j list chain`'s schema to read back
// each rule's handle and jump target.
type nftRuleset struct {
	Nftables []struct {
		Rule *struct {
			Handle int              `json:"handle"`
			Expr   []map[string]any `json:"expr"`
		} `json:"rule"`
	} `json:"nftables"`
}

// jumpTargets returns the set of chain names chain already jumps to (via
// any rule) -- ensureJumpRules only needs presence, not the rule handle
// jumpHandles also returns (that's Remove's own use).
func jumpTargets(chain string) (map[string]bool, error) {
	handles, err := jumpHandles(chain)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(handles))
	for target := range handles {
		out[target] = true
	}
	return out, nil
}

func jumpHandles(chain string) (map[string]int, error) {
	out, err := exec.Command("nft", "-j", "list", "chain", "bridge", table, chain).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return map[string]int{}, nil // table/chain doesn't exist yet
		}
		return nil, fmt.Errorf("nftacl: nft -j list chain bridge %s %s: %w: %s", table, chain, err, strings.TrimSpace(string(out)))
	}
	var parsed nftRuleset
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("nftacl: parse nft -j list chain output: %w", err)
	}
	handles := make(map[string]int)
	for _, item := range parsed.Nftables {
		if item.Rule == nil {
			continue
		}
		for _, e := range item.Rule.Expr {
			jump, ok := e["jump"].(map[string]any)
			if !ok {
				continue
			}
			target, _ := jump["target"].(string)
			if target != "" {
				handles[target] = item.Rule.Handle
			}
		}
	}
	return handles, nil
}

// Remove deletes tapName's two base-chain jump rules (looked up by handle,
// see jumpHandles) and its two now-unreferenced chains. No-op for whatever
// part is already absent, so safe to call more than once.
func Remove(tapName string) error {
	handles, err := jumpHandles(baseChain)
	if err != nil {
		return err
	}
	var del strings.Builder
	if h, ok := handles[inChain(tapName)]; ok {
		fmt.Fprintf(&del, "delete rule bridge %s %s handle %d\n", table, baseChain, h)
	}
	if h, ok := handles[outChain(tapName)]; ok {
		fmt.Fprintf(&del, "delete rule bridge %s %s handle %d\n", table, baseChain, h)
	}
	if del.Len() > 0 {
		if err := run(del.String()); err != nil {
			return err
		}
	}

	var errs []error
	for _, chain := range []string{inChain(tapName), outChain(tapName)} {
		script := fmt.Sprintf("delete chain bridge %s %s\n", table, chain)
		if err := run(script); err != nil && !strings.Contains(err.Error(), "No such file or directory") {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("nftacl: remove %s: %v", tapName, errs)
	}
	return nil
}

func run(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nftacl: nft -f -: %w: %s\nscript:\n%s", err, strings.TrimSpace(string(out)), script)
	}
	return nil
}
