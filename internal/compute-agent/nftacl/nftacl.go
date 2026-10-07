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
// `accept` would) for each allow rule (SecurityGroup rules are allow-only),
// or a terminal `drop` for an unmatched packet (default-deny once inside a
// tap's own chain).
//
// # Address sets
//
// A rule whose peer is a SecurityGroup or a Network names an address set
// ("sg:<id>", "network:<id>") instead of carrying addresses: it becomes
// `ip daddr @<set>` / `ip saddr @<set>` against a named nftables set of
// that table (see setName), shared by every tap whose rules reference it.
// A membership change is then one set update (UpdateSets), not a rewrite
// of every referencing tap's chains -- see docs/specs/snap.md. Neither jump rule nor its target chain ever issues
// `accept` itself -- the base chain's own `policy accept` is what finally
// allows a packet once every relevant tap's chain has returned instead of
// dropping.
//
// # Anti-spoofing
//
// A second base chain (bridge/prerouting, also policy accept) holds one
// more guard rule per tap, `iifname "<tap>" jump <tap>-spoof`, enforcing
// that the VM only ever sends from its own allocated MAC/IPv4 (see
// writeAntiSpoof). It lives on prerouting, not forward, because frames a
// VM sends to the bridge itself (its gateway_ip) are delivered locally and
// never reach the forward hook -- a source check only on forward would
// leave the routed path wide open.
//
// # Routed traffic
//
// The bridge forward hook only sees frames bridged between two ports of
// the same bridge. Traffic the host routes -- a VM leaving its Subnet via
// its gateway_ip (which lives on the bridge itself), or one bridge to
// another when ip_forward is on (Docker, for one, turns it on) -- never
// passes it, and would otherwise skip the SecurityGroup rules
// entirely, the host quietly acting as a router between tenants. So the
// same per-tap chains are written again into an inet-family table, hooked
// on forward/input/output and keyed by (bridge, VM IP) rather than the
// tap (see ensureRoutedJumps). Bridged traffic that br_netfilter also
// hands to the inet hooks just gets the same verdict twice.
package nftacl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Rule mirrors snap's vmm.PolicyRule -- its own copy, not an import, same
// "each layer has its own mirror struct" convention as netsetup's own
// types. Allow-only: traffic to/from CIDR or any member of Set (exactly
// one), optionally narrowed to Protocol ("": any) and PortRange
// (destination port(s), tcp/udp; "": all).
type Rule struct {
	Protocol  string
	PortRange string
	CIDR      string
	Set       string
}

// SetUpdate changes one named address set: Full replaces its members,
// otherwise Add/Remove apply.
type SetUpdate struct {
	Name    string
	Full    bool
	Members []string
	Add     []string
	Remove  []string
}

type Interface struct {
	TapName   string
	GatewayIP string
	// IPAddress/MACAddress are the VM's own allocated address on this
	// interface -- the only source IPv4/MAC (including ARP sender fields)
	// the anti-spoofing chain lets the VM send from (see writeAntiSpoof).
	IPAddress  string
	MACAddress string

	IngressRules []Rule
	EgressRules  []Rule
	// Sets are full contents to install (sets the rules reference but
	// that aren't listed here keep whatever members they already have).
	Sets []SetUpdate
}

const (
	table     = "kyuusha_acl"
	baseChain = "base"
	// spoofBaseChain hooks bridge prerouting rather than forward: it must
	// see every frame the VM sends into the bridge, including frames
	// addressed to the bridge itself (the Subnet's gateway_ip, and so
	// anything the host routes onward), which never traverse forward.
	spoofBaseChain = "antispoof"

	// inet-family base chains: the same per-tap rules again, for traffic
	// the host routes (or terminates/originates) rather than bridges --
	// see the package doc comment's "Routed traffic".
	inetFwdChain = "routed_forward"
	inetInChain  = "routed_input"
	inetOutChain = "routed_output"
)

func inChain(tap string) string    { return tap + "-in" }    // iifname == tap -- enforces EgressRules
func outChain(tap string) string   { return tap + "-out" }   // oifname == tap -- enforces IngressRules
func spoofChain(tap string) string { return tap + "-spoof" } // iifname == tap, prerouting -- see writeAntiSpoof

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
	fmt.Fprintf(&setup, "add chain bridge %s %s { type filter hook prerouting priority -200; policy accept; }\n", table, spoofBaseChain)
	fmt.Fprintf(&setup, "add table inet %s\n", table)
	for _, c := range []struct{ name, hook string }{{inetFwdChain, "forward"}, {inetInChain, "input"}, {inetOutChain, "output"}} {
		fmt.Fprintf(&setup, "add chain inet %s %s { type filter hook %s priority 0; policy accept; }\n", table, c.name, c.hook)
	}
	fmt.Fprintf(&setup, "add chain inet %s %s\n", table, inChain(iface.TapName))
	fmt.Fprintf(&setup, "add chain inet %s %s\n", table, outChain(iface.TapName))
	for _, name := range referencedSets(iface) {
		for _, fam := range []string{"bridge", "inet"} {
			fmt.Fprintf(&setup, "add set %s %s %s { type ipv4_addr; }\n", fam, table, setName(name))
		}
	}
	for _, u := range iface.Sets {
		writeSetContents(&setup, u.Name, u.Members)
	}
	if err := run(setup.String()); err != nil {
		return err
	}

	var rules strings.Builder
	writeTapChains(&rules, "bridge", iface)
	writeTapChains(&rules, "inet", iface)
	// Unknown address (either one empty) leaves any previously installed
	// anti-spoofing chain untouched rather than flushing it: re-Apply's
	// only post-boot caller (the policy stream) changes rules, never the
	// interface's address, so "don't know" must never mean "stop checking".
	antiSpoof := iface.IPAddress != "" && iface.MACAddress != ""
	if antiSpoof {
		fmt.Fprintf(&rules, "add chain bridge %s %s\n", table, spoofChain(iface.TapName))
		fmt.Fprintf(&rules, "flush chain bridge %s %s\n", table, spoofChain(iface.TapName))
		writeAntiSpoof(&rules, spoofChain(iface.TapName), iface.IPAddress, iface.MACAddress)
	}
	if err := run(rules.String()); err != nil {
		return err
	}

	if err := ensureJumpRules("bridge", baseChain, map[string]string{
		inChain(iface.TapName):  fmt.Sprintf("iifname %q", iface.TapName),
		outChain(iface.TapName): fmt.Sprintf("oifname %q", iface.TapName),
	}); err != nil {
		return err
	}
	if !antiSpoof {
		return nil
	}
	if err := ensureJumpRules("bridge", spoofBaseChain, map[string]string{
		spoofChain(iface.TapName): fmt.Sprintf("iifname %q", iface.TapName),
	}); err != nil {
		return err
	}
	return ensureRoutedJumps(iface.TapName, iface.IPAddress)
}

// ensureRoutedJumps hooks tapName's inet-family chains into routed
// traffic. Past the bridge, the tap itself is no longer visible -- a
// routed packet's interface is the bridge -- so the VM is identified by
// (bridge, its own IP) instead; anti-spoofing (bridge prerouting) has
// already guaranteed nothing else on that bridge sends from that IP. A
// tap with no bridge master (a non-bridge VNAP wiring, which nftacl
// doesn't support anyway) gets no routed jumps.
func ensureRoutedJumps(tapName, ip string) error {
	target, err := os.Readlink(filepath.Join("/sys/class/net", tapName, "master"))
	if err != nil {
		return nil
	}
	br := filepath.Base(target)
	from := fmt.Sprintf("iifname %q ip saddr %s", br, ip) // the VM's own outbound traffic
	to := fmt.Sprintf("oifname %q ip daddr %s", br, ip)   // traffic about to be delivered to the VM
	if err := ensureJumpRules("inet", inetFwdChain, map[string]string{inChain(tapName): from, outChain(tapName): to}); err != nil {
		return err
	}
	if err := ensureJumpRules("inet", inetInChain, map[string]string{inChain(tapName): from}); err != nil {
		return err
	}
	return ensureJumpRules("inet", inetOutChain, map[string]string{outChain(tapName): to})
}

// writeTapChains (re)writes tapName's -in/-out chains in fam's table:
// baseline, then the allow rules, then default drop.
func writeTapChains(b *strings.Builder, fam string, iface Interface) {
	in, out := inChain(iface.TapName), outChain(iface.TapName)
	fmt.Fprintf(b, "flush chain %s %s %s\n", fam, table, in)
	fmt.Fprintf(b, "flush chain %s %s %s\n", fam, table, out)
	// The baseline names the *peer*, like writeRule: the destination of the
	// VM's outbound traffic, the source of traffic delivered to it.
	// (Matching the VM's own side instead -- saddr on -in, daddr on -out --
	// is always true once anti-spoofing pins the VM to its own IP, which
	// would make every rule below unreachable.)
	writeBaseline(b, fam, in, iface.GatewayIP, "daddr")
	writeBaseline(b, fam, out, iface.GatewayIP, "saddr")
	for _, r := range iface.EgressRules {
		writeRule(b, fam, in, r, "daddr") // VM -> remote peer: peer is the destination
	}
	for _, r := range iface.IngressRules {
		writeRule(b, fam, out, r, "saddr") // remote sender -> VM: peer is the source
	}
	fmt.Fprintf(b, "add rule %s %s %s drop\n", fam, table, in)
	fmt.Fprintf(b, "add rule %s %s %s drop\n", fam, table, out)
}

// writeAntiSpoof fills a tap's prerouting chain: everything the VM sends
// must carry its own allocated MAC as the Ethernet source, and be either
// IPv4 from its own allocated IP, or ARP whose sender fields are its own
// MAC and IP (0.0.0.0 is also allowed as the sender IP: RFC 5227 ARP
// probes). Any other EtherType -- IPv6 (kyuusha allocates no IPv6
// addresses), 802.1Q-tagged frames (a VM must never reach another VLAN by
// tagging its own frames) -- is dropped. Unlike the -in/-out chains this
// is not tenant-configurable: no egress_rules entry can widen it.
func writeAntiSpoof(b *strings.Builder, chain, ip, mac string) {
	fmt.Fprintf(b, "add rule bridge %s %s ether saddr != %s drop\n", table, chain, mac)
	fmt.Fprintf(b, "add rule bridge %s %s ether type ip ip saddr %s return\n", table, chain, ip)
	fmt.Fprintf(b, "add rule bridge %s %s ether type arp arp saddr ether %s arp saddr ip { %s, 0.0.0.0 } return\n", table, chain, mac, ip)
	fmt.Fprintf(b, "add rule bridge %s %s drop\n", table, chain)
}

// writeBaseline adds what every tap chain lets through regardless of its
// SecurityGroups -- the host's own basic plumbing, without which a VM
// can't communicate at all even with groups allowing it: established/
// related connections, ARP, and the Subnet's gateway_ip. Nothing else: in
// particular not the rest of the VM's own Subnet (same-Network traffic is
// a Network's default SecurityGroup's rule, visible and removable).
// `return`, not `accept` -- see the package doc comment for why.
//
// ARP is let through because it isn't IPv4 (no ip saddr/daddr rule
// matches it) and conntrack doesn't track it; whether an ARP frame is
// legitimate is the anti-spoofing chain's job (see writeAntiSpoof).
func writeBaseline(b *strings.Builder, fam, chain, gatewayIP, addrKeyword string) {
	fmt.Fprintf(b, "add rule %s %s %s ct state established,related return\n", fam, table, chain)
	if fam == "bridge" { // inet hooks never see ARP
		fmt.Fprintf(b, "add rule %s %s %s ether type arp return\n", fam, table, chain)
	}
	if gatewayIP != "" {
		fmt.Fprintf(b, "add rule %s %s %s ip %s %s return\n", fam, table, chain, addrKeyword, gatewayIP)
	}
}

func writeRule(b *strings.Builder, fam, chain string, r Rule, addrKeyword string) {
	var match []string
	v6 := false
	switch {
	case r.Set != "":
		match = append(match, fmt.Sprintf("ip %s @%s", addrKeyword, setName(r.Set)))
	case r.CIDR != "":
		ip, _, err := net.ParseCIDR(r.CIDR)
		if err != nil {
			return // validated upstream; skip rather than emit a malformed rule
		}
		if ip.To4() == nil {
			v6 = true
			match = append(match, fmt.Sprintf("ip6 %s %s", addrKeyword, r.CIDR))
		} else {
			match = append(match, fmt.Sprintf("ip %s %s", addrKeyword, r.CIDR))
		}
	default:
		return
	}
	switch r.Protocol {
	case "tcp", "udp":
		switch {
		case r.PortRange != "":
			match = append(match, fmt.Sprintf("%s dport %s", r.Protocol, r.PortRange))
		case v6:
			match = append(match, "meta l4proto "+r.Protocol)
		default:
			match = append(match, "ip protocol "+r.Protocol)
		}
	case "icmp":
		if v6 {
			match = append(match, "meta l4proto ipv6-icmp")
		} else {
			match = append(match, "ip protocol icmp")
		}
	case "":
	default:
		return // validated upstream
	}
	fmt.Fprintf(b, "add rule %s %s %s %s return\n", fam, table, chain, strings.Join(match, " "))
}

// setName is the nftables name of address set name: a short hash, since
// set names are length-limited and "sg:<id>" has a character nft doesn't
// allow there.
func setName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "ks_" + hex.EncodeToString(sum[:6])
}

func referencedSets(iface Interface) []string {
	var out []string
	for _, rs := range [][]Rule{iface.IngressRules, iface.EgressRules} {
		for _, r := range rs {
			if r.Set != "" && !slices.Contains(out, r.Set) {
				out = append(out, r.Set)
			}
		}
	}
	for _, u := range iface.Sets {
		if !slices.Contains(out, u.Name) {
			out = append(out, u.Name)
		}
	}
	return out
}

// writeSetContents replaces set name's members (in both tables).
// Non-IPv4 members are skipped: the sets are ipv4_addr.
func writeSetContents(b *strings.Builder, name string, members []string) {
	var elems []string
	for _, m := range members {
		if ip := net.ParseIP(m); ip != nil && ip.To4() != nil {
			elems = append(elems, m)
		}
	}
	for _, fam := range []string{"bridge", "inet"} {
		fmt.Fprintf(b, "flush set %s %s %s\n", fam, table, setName(name))
		if len(elems) > 0 {
			fmt.Fprintf(b, "add element %s %s %s { %s }\n", fam, table, setName(name), strings.Join(elems, ", "))
		}
	}
}

// UpdateSets applies address-set changes to the sets this host has (one
// some tap's rules reference); others are skipped. A delta is applied by
// reading the current members and rewriting the set, all in one nft
// transaction per set.
func UpdateSets(updates []SetUpdate) error {
	for _, u := range updates {
		current, ok, err := setElements(setName(u.Name))
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		members := u.Members
		if !u.Full {
			members = current
			for _, a := range u.Add {
				if !slices.Contains(members, a) {
					members = append(members, a)
				}
			}
			members = slices.DeleteFunc(members, func(m string) bool { return slices.Contains(u.Remove, m) })
		}
		var b strings.Builder
		writeSetContents(&b, u.Name, members)
		if err := run(b.String()); err != nil {
			return err
		}
	}
	return nil
}

// setElements reads a set's members from the bridge table; ok=false if
// the set doesn't exist.
func setElements(nftName string) ([]string, bool, error) {
	out, err := exec.Command("nft", "-j", "list", "set", "bridge", table, nftName).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("nftacl: nft -j list set %s: %w: %s", nftName, err, strings.TrimSpace(string(out)))
	}
	var parsed struct {
		Nftables []struct {
			Set *struct {
				Elem []any `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, false, fmt.Errorf("nftacl: parse nft -j list set output: %w", err)
	}
	var elems []string
	for _, item := range parsed.Nftables {
		if item.Set == nil {
			continue
		}
		for _, e := range item.Set.Elem {
			if s, ok := e.(string); ok {
				elems = append(elems, s)
			}
		}
	}
	return elems, true, nil
}

// removeUnusedSets deletes every address set no rule references any more
// (nft refuses to delete one still in use, which is exactly the check).
func removeUnusedSets() {
	for _, fam := range []string{"bridge", "inet"} {
		out, err := exec.Command("nft", "-j", "list", "sets", fam).CombinedOutput()
		if err != nil {
			continue
		}
		var parsed struct {
			Nftables []struct {
				Set *struct {
					Name  string `json:"name"`
					Table string `json:"table"`
				} `json:"set"`
			} `json:"nftables"`
		}
		if json.Unmarshal(out, &parsed) != nil {
			continue
		}
		for _, item := range parsed.Nftables {
			if item.Set != nil && item.Set.Table == table && strings.HasPrefix(item.Set.Name, "ks_") {
				_ = run(fmt.Sprintf("delete set %s %s %s\n", fam, table, item.Set.Name))
			}
		}
	}
}

// ensureJumpRules adds one "<match> jump <target>" rule to base for each
// entry of jumps (target chain -> match expression) unless base already
// jumps to that target -- checked via `nft -j list chain`, since a plain
// `add rule` isn't idempotent the way `add table`/`add chain` are (it would
// append a duplicate jump on every call, e.g. every policy re-apply
// over a VM's lifetime).
func ensureJumpRules(fam, base string, jumps map[string]string) error {
	existing, err := jumpTargets(fam, base)
	if err != nil {
		return err
	}
	var add strings.Builder
	for target, match := range jumps {
		if !existing[target] {
			fmt.Fprintf(&add, "add rule %s %s %s %s jump %s\n", fam, table, base, match, target)
		}
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
func jumpTargets(fam, chain string) (map[string]bool, error) {
	handles, err := jumpHandles(fam, chain)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(handles))
	for target := range handles {
		out[target] = true
	}
	return out, nil
}

func jumpHandles(fam, chain string) (map[string]int, error) {
	out, err := exec.Command("nft", "-j", "list", "chain", fam, table, chain).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return map[string]int{}, nil // table/chain doesn't exist yet
		}
		return nil, fmt.Errorf("nftacl: nft -j list chain %s %s %s: %w: %s", fam, table, chain, err, strings.TrimSpace(string(out)))
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

// Remove deletes tapName's base-chain jump rules (looked up by handle, see
// jumpHandles) and its now-unreferenced chains. No-op for whatever part is
// already absent, so safe to call more than once.
func Remove(tapName string) error {
	in, out, spoof := inChain(tapName), outChain(tapName), spoofChain(tapName)
	var del strings.Builder
	for _, j := range []struct {
		fam, base string
		targets   []string
	}{
		{"bridge", baseChain, []string{in, out}},
		{"bridge", spoofBaseChain, []string{spoof}},
		{"inet", inetFwdChain, []string{in, out}},
		{"inet", inetInChain, []string{in}},
		{"inet", inetOutChain, []string{out}},
	} {
		handles, err := jumpHandles(j.fam, j.base)
		if err != nil {
			return err
		}
		for _, target := range j.targets {
			if h, ok := handles[target]; ok {
				fmt.Fprintf(&del, "delete rule %s %s %s handle %d\n", j.fam, table, j.base, h)
			}
		}
	}
	if del.Len() > 0 {
		if err := run(del.String()); err != nil {
			return err
		}
	}

	var errs []error
	for _, c := range []struct{ fam, chain string }{{"bridge", in}, {"bridge", out}, {"bridge", spoof}, {"inet", in}, {"inet", out}} {
		script := fmt.Sprintf("delete chain %s %s %s\n", c.fam, table, c.chain)
		if err := run(script); err != nil && !strings.Contains(err.Error(), "No such file or directory") {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("nftacl: remove %s: %v", tapName, errs)
	}
	removeUnusedSets()
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
