// Command ebpf-snap is a reference implementation of kyuusha's
// security-backend plugin contract (internal/compute-agent/snap),
// enforcing NetworkInterface ingress_rules/egress_rules natively in eBPF
// (TC-BPF, attached directly to the VM's tap device) instead of nftacl's
// default bridge-family nftables. Unlike nftacl, this does not require the
// tap to be a Linux bridge port, so it also works with non-bridge VNAP tap
// wiring (e.g. examples/vnap-plugins/frr-type5.sh's EVPN Type-5 setup).
//
// This is the stateful version: it tracks established flows itself (see
// bpf/snap.c's conntrack map), since TC-BPF hooks have no access to
// netfilter's own conntrack. A future, separate stateless/performance-
// focused plugin is expected to trade this away for raw throughput -- see
// this package's README for the full trade-off discussion.
//
// Same contract as every other security-backend plugin: exec'd as
// "<bin> attach" or "<bin> detach" with a JSON payload on stdin, success is
// exit code 0 only. See internal/compute-agent/snap's pluginRequest for
// the authoritative shape this mirrors.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// pinRoot holds every tap's pinned maps/links plus the one host-wide
// shared conntrack map -- see bpf/snap.c's own doc comment for why
// conntrack is shared across taps but rules_ingress/rules_egress are not.
const pinRoot = "/sys/fs/bpf/kyuusha-snap"

// firewallRule mirrors snap.pluginFirewallRule exactly (internal/
// compute-agent/snap/snap.go) -- the wire shape is part of the
// contract, not something this plugin gets to redefine.
type firewallRule struct {
	Protocol   string `json:"protocol"`
	PortRange  string `json:"port_range,omitempty"`
	SourceCIDR string `json:"source_cidr"`
	Action     string `json:"action"`
}

// pluginRequest mirrors snap.pluginRequest exactly.
type pluginRequest struct {
	TapName    string `json:"tap_name"`
	IfaceID    string `json:"iface_id"`
	VMID       string `json:"vm_id"`
	TenantID   string `json:"tenant_id"`
	SubnetCIDR string `json:"subnet_cidr,omitempty"`
	GatewayIP  string `json:"gateway_ip,omitempty"`

	IngressRules []firewallRule `json:"ingress_rules,omitempty"`
	EgressRules  []firewallRule `json:"egress_rules,omitempty"`
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "attach" && os.Args[1] != "detach") {
		fmt.Fprintln(os.Stderr, "usage: ebpf-snap attach|detach  (JSON payload on stdin)")
		os.Exit(2)
	}

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatalf("read stdin: %v", err)
	}
	var req pluginRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		fatalf("parse request: %v", err)
	}
	if req.TapName == "" {
		fatalf("tap_name is required")
	}

	switch os.Args[1] {
	case "attach":
		err = attach(req)
	case "detach":
		err = detach(req)
	}
	if err != nil {
		fatalf("%s %s: %v", os.Args[1], req.TapName, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ebpf-snap: "+format+"\n", args...)
	os.Exit(1)
}

func tapPinDir(tapName string) string { return filepath.Join(pinRoot, tapName) }

// attach installs (first call for this tap) or re-populates (every later
// call, e.g. a UpdateFirewallRules-triggered re-apply) this tap's ACL
// state. Re-population only rewrites the two rule maps' contents -- the
// already-attached TC programs keep running throughout, so there is no
// enforcement gap during an update, unlike detach+reattach would cause.
func attach(req pluginRequest) error {
	dir := tapPinDir(req.TapName)
	rulesIngressPin := filepath.Join(dir, "rules_ingress")
	rulesEgressPin := filepath.Join(dir, "rules_egress")

	if _, err := os.Stat(rulesIngressPin); err == nil {
		// Already attached for this tap -- just rewrite the rule maps.
		rulesIngress, err := ebpf.LoadPinnedMap(rulesIngressPin, nil)
		if err != nil {
			return fmt.Errorf("load pinned rules_ingress: %w", err)
		}
		defer rulesIngress.Close()
		rulesEgress, err := ebpf.LoadPinnedMap(rulesEgressPin, nil)
		if err != nil {
			return fmt.Errorf("load pinned rules_egress: %w", err)
		}
		defer rulesEgress.Close()
		if err := populateRules(rulesIngress, req.SubnetCIDR, req.GatewayIP, req.IngressRules); err != nil {
			return fmt.Errorf("update rules_ingress: %w", err)
		}
		if err := populateRules(rulesEgress, req.SubnetCIDR, req.GatewayIP, req.EgressRules); err != nil {
			return fmt.Errorf("update rules_egress: %w", err)
		}
		return nil
	}

	iface, err := net.InterfaceByName(req.TapName)
	if err != nil {
		return fmt.Errorf("lookup tap: %w", err)
	}

	spec, err := loadBpf()
	if err != nil {
		return fmt.Errorf("load BPF spec: %w", err)
	}

	conntrackMap, err := loadOrCreateSharedConntrack(spec)
	if err != nil {
		return fmt.Errorf("shared conntrack map: %w", err)
	}
	defer conntrackMap.Close()

	var objs bpfObjects
	if err := spec.LoadAndAssign(&objs, &ebpf.CollectionOptions{
		MapReplacements: map[string]*ebpf.Map{"conntrack": conntrackMap},
	}); err != nil {
		return fmt.Errorf("load collection: %w", err)
	}
	defer objs.EnforceVmEgress.Close()
	defer objs.EnforceVmIngress.Close()

	if err := populateRules(objs.RulesIngress, req.SubnetCIDR, req.GatewayIP, req.IngressRules); err != nil {
		objs.RulesIngress.Close()
		objs.RulesEgress.Close()
		return fmt.Errorf("populate rules_ingress: %w", err)
	}
	if err := populateRules(objs.RulesEgress, req.SubnetCIDR, req.GatewayIP, req.EgressRules); err != nil {
		objs.RulesIngress.Close()
		objs.RulesEgress.Close()
		return fmt.Errorf("populate rules_egress: %w", err)
	}

	// TCX: a modern (qdisc-free) attach point, unlike the classic tc/
	// clsact BPF attachment -- no `tc qdisc add ... clsact` needed first.
	egressLink, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   objs.EnforceVmEgress,
		Attach:    ebpf.AttachTCXIngress, // TC ingress on the tap == the VM's own outbound traffic
	})
	if err != nil {
		objs.RulesIngress.Close()
		objs.RulesEgress.Close()
		return fmt.Errorf("attach enforce_vm_egress to tcx ingress: %w", err)
	}
	ingressLink, err := link.AttachTCX(link.TCXOptions{
		Interface: iface.Index,
		Program:   objs.EnforceVmIngress,
		Attach:    ebpf.AttachTCXEgress, // TC egress on the tap == traffic delivered to the VM
	})
	if err != nil {
		egressLink.Close()
		objs.RulesIngress.Close()
		objs.RulesEgress.Close()
		return fmt.Errorf("attach enforce_vm_ingress to tcx egress: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir pin dir: %w", err)
	}
	if err := objs.RulesIngress.Pin(rulesIngressPin); err != nil {
		return fmt.Errorf("pin rules_ingress: %w", err)
	}
	if err := objs.RulesEgress.Pin(rulesEgressPin); err != nil {
		return fmt.Errorf("pin rules_egress: %w", err)
	}
	if err := egressLink.Pin(filepath.Join(dir, "link_vm_egress")); err != nil {
		return fmt.Errorf("pin enforce_vm_egress link: %w", err)
	}
	if err := ingressLink.Pin(filepath.Join(dir, "link_vm_ingress")); err != nil {
		return fmt.Errorf("pin enforce_vm_ingress link: %w", err)
	}
	return nil
}

// loadOrCreateSharedConntrack loads the host-wide conntrack map if some
// earlier attach (for any tap) already created it, else creates and pins a
// fresh one from spec's own map definition.
func loadOrCreateSharedConntrack(spec *ebpf.CollectionSpec) (*ebpf.Map, error) {
	pin := filepath.Join(pinRoot, "conntrack")
	if m, err := ebpf.LoadPinnedMap(pin, nil); err == nil {
		return m, nil
	}
	if err := os.MkdirAll(pinRoot, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir pin root: %w", err)
	}
	m, err := ebpf.NewMap(spec.Maps["conntrack"])
	if err != nil {
		return nil, fmt.Errorf("create conntrack map: %w", err)
	}
	if err := m.Pin(pin); err != nil {
		m.Close()
		return nil, fmt.Errorf("pin conntrack map: %w", err)
	}
	return m, nil
}

// populateRules writes subnetCIDR/gatewayIP as the two implicit baseline
// allow entries (mirroring nftacl.writeBaseline), then rules in order,
// then zeroes every remaining slot -- an ebpf.MAP_TYPE_ARRAY always holds
// a value in every index, so a re-Apply with fewer rules than before must
// explicitly clear the leftover slots rather than just not writing them.
func populateRules(m *ebpf.Map, subnetCIDR, gatewayIP string, rules []firewallRule) error {
	entries := make([]bpfRule, 0, maxRules)
	if subnetCIDR != "" {
		r, err := cidrRule(subnetCIDR)
		if err != nil {
			return fmt.Errorf("subnet_cidr %q: %w", subnetCIDR, err)
		}
		entries = append(entries, r)
	}
	if gatewayIP != "" {
		r, err := cidrRule(gatewayIP + "/32")
		if err != nil {
			return fmt.Errorf("gateway_ip %q: %w", gatewayIP, err)
		}
		entries = append(entries, r)
	}
	for _, fr := range rules {
		r, err := ruleFromFirewallRule(fr)
		if err != nil {
			return err
		}
		entries = append(entries, r)
	}
	if len(entries) > maxRules {
		return fmt.Errorf("%d rules (including baseline) exceeds the %d-entry limit", len(entries), maxRules)
	}

	for i := 0; i < maxRules; i++ {
		var v bpfRule
		if i < len(entries) {
			v = entries[i]
		}
		if err := m.Put(uint32(i), v); err != nil {
			return fmt.Errorf("write rule slot %d: %w", i, err)
		}
	}
	return nil
}

// maxRules must match bpf/snap.c's MAX_RULES.
const maxRules = 64

// CidrAddr/CidrMask must be built with binary.NativeEndian, not
// binary.BigEndian: cilium/ebpf serializes a Go struct's fields into the
// map's raw bytes using the host's native byte order (bpf2go generates a
// bpfeb/bpfel variant pair for exactly this reason), so the only way the
// raw bytes stored in the map end up matching bpf/snap.c's un-converted
// (network-order) ip->saddr/daddr comparison is to pack them natively here
// too -- using BigEndian on a little-endian host silently byte-swaps every
// address and breaks all matching (found via hands-on veth testing, not
// something a unit test alone would have caught).
func cidrRule(cidr string) (bpfRule, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return bpfRule{}, err
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return bpfRule{}, fmt.Errorf("only IPv4 is supported, got %q", cidr)
	}
	mask := ipnet.Mask
	return bpfRule{
		CidrAddr: binary.NativeEndian.Uint32(ip4),
		CidrMask: binary.NativeEndian.Uint32(mask),
		PortLo:   0,
		PortHi:   65535,
		Protocol: 0, // any
		Action:   1, // allow
		Active:   1,
	}, nil
}

func ruleFromFirewallRule(fr firewallRule) (bpfRule, error) {
	var protocol uint8
	switch fr.Protocol {
	case "tcp":
		protocol = 6
	case "udp":
		protocol = 17
	case "icmp":
		protocol = 1
	default:
		return bpfRule{}, fmt.Errorf("unrecognized protocol %q", fr.Protocol)
	}
	var action uint8
	switch fr.Action {
	case "allow":
		action = 1
	case "deny":
		action = 0
	default:
		return bpfRule{}, fmt.Errorf("unrecognized action %q", fr.Action)
	}
	portLo, portHi := uint16(0), uint16(65535)
	if protocol != 1 { // not icmp
		var err error
		portLo, portHi, err = parsePortRange(fr.PortRange)
		if err != nil {
			return bpfRule{}, fmt.Errorf("port_range %q: %w", fr.PortRange, err)
		}
	}
	_, ipnet, err := net.ParseCIDR(fr.SourceCIDR)
	if err != nil {
		return bpfRule{}, fmt.Errorf("source_cidr %q: %w", fr.SourceCIDR, err)
	}
	ip4 := ipnet.IP.To4()
	if ip4 == nil {
		return bpfRule{}, fmt.Errorf("only IPv4 is supported, got %q", fr.SourceCIDR)
	}
	return bpfRule{
		CidrAddr: binary.NativeEndian.Uint32(ip4),
		CidrMask: binary.NativeEndian.Uint32(ipnet.Mask),
		PortLo:   portLo,
		PortHi:   portHi,
		Protocol: protocol,
		Action:   action,
		Active:   1,
	}, nil
}

// parsePortRange accepts "22" or "2379-2380", same wire format
// internal/network's validatePortRange already validated upstream.
func parsePortRange(s string) (uint16, uint16, error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		p, err := strconv.ParseUint(s, 10, 16)
		if err != nil {
			return 0, 0, err
		}
		return uint16(p), uint16(p), nil
	}
	loN, err := strconv.ParseUint(lo, 10, 16)
	if err != nil {
		return 0, 0, err
	}
	hiN, err := strconv.ParseUint(hi, 10, 16)
	if err != nil {
		return 0, 0, err
	}
	return uint16(loN), uint16(hiN), nil
}

// detach removes this tap's TC links and rule maps. A pinned bpf_link (or
// map) is kept alive by the kernel exactly as long as it has a reference --
// an open fd, or its bpffs pin; a fresh CLI process never holds an fd open,
// so simply unlinking the pin directory drops the links' and maps' last
// reference and the kernel detaches/frees them itself, no separate
// load-then-Close step needed. The shared conntrack map is left untouched
// (see bpf/snap.c's doc comment: flows aren't tap-scoped, and stale
// entries age out via CONNTRACK_TIMEOUT_NS anyway -- the same non-cleanup
// nftacl itself accepts for its own ct state table).
func detach(req pluginRequest) error {
	dir := tapPinDir(req.TapName)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil // never attached, or already detached -- safe to call twice
	}
	return os.RemoveAll(dir)
}
