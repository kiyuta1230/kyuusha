// Command ebpf-snap is a reference implementation of kyuusha's
// security-backend plugin contract (internal/compute-agent/snap),
// enforcing a NetworkInterface's SecurityGroup rules natively in eBPF
// (TC-BPF, attached directly to the VM's tap device) instead of nftacl's
// default bridge-family nftables. Unlike nftacl, this does not require the
// tap to be a Linux bridge port, so it also works with non-bridge VNAP tap
// wiring (e.g. examples/vnap-plugins/frr-vrf-host-route.sh's pure-L3 setup).
//
// This is the stateful version: it tracks established flows itself (see
// bpf/snap.c's conntrack map), since TC-BPF hooks have no access to
// netfilter's own conntrack. A future, separate stateless/performance-
// focused plugin is expected to trade this away for raw throughput -- see
// this package's README for the full trade-off discussion.
//
// Same contract as every other security-backend plugin: exec'd as
// "<bin> attach", "<bin> detach" or "<bin> update_sets" with a JSON payload
// on stdin, success is exit code 0 only. See docs/specs/snap.md and
// internal/compute-agent/snap's PluginRequest for the authoritative shape
// this mirrors.
//
// Address sets ("sg:<id>", "network:<id>") live in one host-wide hash map
// (set_members, keyed by a 32-bit id derived from the set's name -- see
// setID) that every tap's rules consult, so update_sets is a handful of
// map writes. Members of sets no tap references any more are not garbage
// collected (a reference implementation's simplification).
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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

// policyRule/setUpdate/pluginRequest mirror internal/compute-agent/snap's
// PluginRequest (and vmm.PolicyRule/vmm.SetUpdate) exactly -- the wire
// shape is part of the contract, not something this plugin gets to
// redefine.
type policyRule struct {
	Protocol  string `json:"protocol,omitempty"`
	PortRange string `json:"port_range,omitempty"`
	CIDR      string `json:"cidr,omitempty"`
	Set       string `json:"set,omitempty"`
}

type setUpdate struct {
	Name    string   `json:"name"`
	Version int64    `json:"version"`
	Full    bool     `json:"full,omitempty"`
	Members []string `json:"members,omitempty"`
	Add     []string `json:"add,omitempty"`
	Remove  []string `json:"remove,omitempty"`
}

type pluginRequest struct {
	TapName    string `json:"tap_name,omitempty"`
	IfaceID    string `json:"iface_id,omitempty"`
	VMID       string `json:"vm_id,omitempty"`
	TenantID   string `json:"tenant_id,omitempty"`
	SubnetID   string `json:"subnet_id,omitempty"`
	SubnetCIDR string `json:"subnet_cidr,omitempty"`
	GatewayIP  string `json:"gateway_ip,omitempty"`
	IPAddress  string `json:"ip_address,omitempty"`
	MACAddress string `json:"mac_address,omitempty"`

	SecurityGroupIDs []string     `json:"security_group_ids,omitempty"`
	IngressRules     []policyRule `json:"ingress_rules,omitempty"`
	EgressRules      []policyRule `json:"egress_rules,omitempty"`
	// attach: full contents of the sets the rules reference; update_sets:
	// the changes.
	Sets []setUpdate `json:"sets,omitempty"`
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "attach" && os.Args[1] != "detach" && os.Args[1] != "update_sets") {
		fmt.Fprintln(os.Stderr, "usage: ebpf-snap attach|detach|update_sets  (JSON payload on stdin)")
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
	if req.TapName == "" && os.Args[1] != "update_sets" {
		fatalf("tap_name is required")
	}

	switch os.Args[1] {
	case "attach":
		err = attach(req)
	case "detach":
		err = detach(req)
	case "update_sets":
		err = updateSets(req.Sets)
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
// call, e.g. a re-apply after a policy change) this tap's ACL
// state. Re-population only rewrites the two rule maps' contents -- the
// already-attached TC programs keep running throughout, so there is no
// enforcement gap during an update, unlike detach+reattach would cause.
func attach(req pluginRequest) error {
	dir := tapPinDir(req.TapName)
	rulesIngressPin := filepath.Join(dir, "rules_ingress")
	rulesEgressPin := filepath.Join(dir, "rules_egress")
	spoofPin := filepath.Join(dir, "spoof")

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
		if err := updateSets(fullCopies(req.Sets)); err != nil {
			return fmt.Errorf("update set_members: %w", err)
		}
		if err := populateRules(rulesIngress, req.GatewayIP, req.IngressRules); err != nil {
			return fmt.Errorf("update rules_ingress: %w", err)
		}
		if err := populateRules(rulesEgress, req.GatewayIP, req.EgressRules); err != nil {
			return fmt.Errorf("update rules_egress: %w", err)
		}
		spoof, err := ebpf.LoadPinnedMap(spoofPin, nil)
		if err != nil {
			return fmt.Errorf("load pinned spoof (tap attached by an older ebpf-snap? detach and re-attach it): %w", err)
		}
		defer spoof.Close()
		if err := populateSpoof(spoof, req.IPAddress, req.MACAddress); err != nil {
			return fmt.Errorf("update spoof: %w", err)
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

	conntrackMap, err := loadOrCreateShared(spec, "conntrack")
	if err != nil {
		return fmt.Errorf("shared conntrack map: %w", err)
	}
	defer conntrackMap.Close()
	setMembers, err := loadOrCreateShared(spec, "set_members")
	if err != nil {
		return fmt.Errorf("shared set_members map: %w", err)
	}
	defer setMembers.Close()
	if err := applySets(setMembers, fullCopies(req.Sets)); err != nil {
		return fmt.Errorf("populate set_members: %w", err)
	}

	var objs bpfObjects
	if err := spec.LoadAndAssign(&objs, &ebpf.CollectionOptions{
		MapReplacements: map[string]*ebpf.Map{"conntrack": conntrackMap, "set_members": setMembers},
	}); err != nil {
		return fmt.Errorf("load collection: %w", err)
	}
	defer objs.EnforceVmEgress.Close()
	defer objs.EnforceVmIngress.Close()
	defer objs.Spoof.Close() // the pin below keeps it alive past this process

	if err := populateSpoof(objs.Spoof, req.IPAddress, req.MACAddress); err != nil {
		objs.RulesIngress.Close()
		objs.RulesEgress.Close()
		return fmt.Errorf("populate spoof: %w", err)
	}

	if err := populateRules(objs.RulesIngress, req.GatewayIP, req.IngressRules); err != nil {
		objs.RulesIngress.Close()
		objs.RulesEgress.Close()
		return fmt.Errorf("populate rules_ingress: %w", err)
	}
	if err := populateRules(objs.RulesEgress, req.GatewayIP, req.EgressRules); err != nil {
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
	if err := objs.Spoof.Pin(spoofPin); err != nil {
		return fmt.Errorf("pin spoof: %w", err)
	}
	if err := egressLink.Pin(filepath.Join(dir, "link_vm_egress")); err != nil {
		return fmt.Errorf("pin enforce_vm_egress link: %w", err)
	}
	if err := ingressLink.Pin(filepath.Join(dir, "link_vm_ingress")); err != nil {
		return fmt.Errorf("pin enforce_vm_ingress link: %w", err)
	}
	return nil
}

// loadOrCreateShared loads the host-wide map name (conntrack or
// set_members) if some earlier attach (for any tap) already created it,
// else creates and pins a fresh one from spec's own map definition.
func loadOrCreateShared(spec *ebpf.CollectionSpec, name string) (*ebpf.Map, error) {
	pin := filepath.Join(pinRoot, name)
	if m, err := ebpf.LoadPinnedMap(pin, nil); err == nil {
		return m, nil
	}
	if err := os.MkdirAll(pinRoot, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir pin root: %w", err)
	}
	m, err := ebpf.NewMap(spec.Maps[name])
	if err != nil {
		return nil, fmt.Errorf("create %s map: %w", name, err)
	}
	if err := m.Pin(pin); err != nil {
		m.Close()
		return nil, fmt.Errorf("pin %s map: %w", name, err)
	}
	return m, nil
}

// setID is the 32-bit id an address set is known by in set_members and
// rules: FNV-1a of its name (0 is reserved for "no set, a CIDR rule").
// A collision would merge two sets' members -- accepted for a reference
// implementation.
func setID(name string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(name))
	if id := h.Sum32(); id != 0 {
		return id
	}
	return 1
}

func fullCopies(sets []setUpdate) []setUpdate {
	out := make([]setUpdate, len(sets))
	for i, u := range sets {
		u.Full = true
		out[i] = u
	}
	return out
}

// updateSets applies update_sets' changes to the pinned set_members map;
// nothing to do if no tap has been attached yet (no map).
func updateSets(updates []setUpdate) error {
	m, err := ebpf.LoadPinnedMap(filepath.Join(pinRoot, "set_members"), nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("load pinned set_members: %w", err)
	}
	defer m.Close()
	return applySets(m, updates)
}

func applySets(m *ebpf.Map, updates []setUpdate) error {
	for _, u := range updates {
		id := setID(u.Name)
		add, remove := u.Add, u.Remove
		if u.Full {
			add = u.Members
			var key bpfSetMemberKey
			var val uint8
			var stale []bpfSetMemberKey
			it := m.Iterate()
			for it.Next(&key, &val) {
				if key.SetId == id {
					stale = append(stale, key)
				}
			}
			if err := it.Err(); err != nil {
				return fmt.Errorf("iterate set_members: %w", err)
			}
			for _, k := range stale {
				_ = m.Delete(k)
			}
		}
		for _, a := range remove {
			if ip := net.ParseIP(a).To4(); ip != nil {
				_ = m.Delete(bpfSetMemberKey{SetId: id, Addr: binary.NativeEndian.Uint32(ip)})
			}
		}
		for _, a := range add {
			if ip := net.ParseIP(a).To4(); ip != nil {
				if err := m.Put(bpfSetMemberKey{SetId: id, Addr: binary.NativeEndian.Uint32(ip)}, uint8(1)); err != nil {
					return fmt.Errorf("add %s to %s: %w", a, u.Name, err)
				}
			}
		}
	}
	return nil
}

// populateRules writes gatewayIP as the implicit baseline allow entry
// (mirroring nftacl.writeBaseline), then rules, then zeroes every
// remaining slot -- an ebpf.MAP_TYPE_ARRAY always holds a value in every
// index, so a re-apply with fewer rules than before must explicitly clear
// the leftover slots rather than just not writing them. IPv6 CIDR rules
// are skipped: this plugin enforces IPv4 only (anti-spoofing drops
// everything else the VM sends).
func populateRules(m *ebpf.Map, gatewayIP string, rules []policyRule) error {
	entries := make([]bpfRule, 0, maxRules)
	if gatewayIP != "" {
		r, err := cidrRule(gatewayIP + "/32")
		if err != nil {
			return fmt.Errorf("gateway_ip %q: %w", gatewayIP, err)
		}
		entries = append(entries, r)
	}
	for _, pr := range rules {
		r, ok, err := ruleFromPolicyRule(pr)
		if err != nil {
			return err
		}
		if ok {
			entries = append(entries, r)
		}
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

// populateSpoof writes the VM's own allocated address into the spoof map
// (bpf/snap.c's source_ok). Either value empty means "unknown": the map is
// left as it is, same as nftacl leaving a previously installed
// anti-spoofing chain alone -- a re-apply changes rules, never the
// interface's address, so it must never switch the check off.
func populateSpoof(m *ebpf.Map, ipAddress, macAddress string) error {
	if ipAddress == "" || macAddress == "" {
		return nil
	}
	ip4 := net.ParseIP(ipAddress).To4()
	if ip4 == nil {
		return fmt.Errorf("ip_address %q is not IPv4", ipAddress)
	}
	mac, err := net.ParseMAC(macAddress)
	if err != nil || len(mac) != 6 {
		return fmt.Errorf("mac_address %q: not a 6-byte MAC", macAddress)
	}
	v := bpfSpoofCfg{Ip: binary.NativeEndian.Uint32(ip4), Active: 1} // NativeEndian: see cidrRule
	copy(v.Mac[:], mac)
	return m.Put(uint32(0), v)
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
		Active:   1,
	}, nil
}

// ruleFromPolicyRule converts one wire rule; ok=false for an IPv6 CIDR
// rule (out of this IPv4-only plugin's scope).
func ruleFromPolicyRule(pr policyRule) (bpfRule, bool, error) {
	var protocol uint8
	switch pr.Protocol {
	case "tcp":
		protocol = 6
	case "udp":
		protocol = 17
	case "icmp":
		protocol = 1
	case "":
		protocol = 0
	default:
		return bpfRule{}, false, fmt.Errorf("unrecognized protocol %q", pr.Protocol)
	}
	portLo, portHi := uint16(0), uint16(65535)
	if (protocol == 6 || protocol == 17) && pr.PortRange != "" {
		var err error
		portLo, portHi, err = parsePortRange(pr.PortRange)
		if err != nil {
			return bpfRule{}, false, fmt.Errorf("port_range %q: %w", pr.PortRange, err)
		}
	}
	r := bpfRule{PortLo: portLo, PortHi: portHi, Protocol: protocol, Active: 1}
	switch {
	case pr.Set != "":
		r.SetId = setID(pr.Set)
	case pr.CIDR != "":
		_, ipnet, err := net.ParseCIDR(pr.CIDR)
		if err != nil {
			return bpfRule{}, false, fmt.Errorf("cidr %q: %w", pr.CIDR, err)
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			return bpfRule{}, false, nil
		}
		r.CidrAddr = binary.NativeEndian.Uint32(ip4)
		r.CidrMask = binary.NativeEndian.Uint32(ipnet.Mask)
	default:
		return bpfRule{}, false, fmt.Errorf("rule has neither cidr nor set")
	}
	return r, true, nil
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
