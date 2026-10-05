//go:build ignore

// snap.c implements kyuusha's security-backend plugin contract
// (internal/compute-agent/snap) natively in eBPF, via TC-BPF attached
// directly to a VM's tap device -- unlike the default nftacl implementation
// (bridge-family nftables), this does NOT require the tap to be a Linux
// bridge port, so it also works with non-bridge VNAP tap wiring (e.g. the
// pure-L3 example, examples/vnap-plugins/frr-vrf-host-route.sh).
//
// # Direction naming (same inversion nftacl documents)
//
// A tap's TC "ingress" hook fires for packets arriving INTO that netdev --
// i.e. packets the VM itself just wrote (the VM's own outbound traffic).
// TC "egress" fires for packets about to leave OUT of that netdev -- i.e.
// packets about to be delivered TO the VM. So:
//   - enforce_vm_egress (attached to TC ingress) enforces egress_rules
//   - enforce_vm_ingress (attached to TC egress) enforces ingress_rules
//
// # Statefulness
//
// nftacl relies on netfilter's own conntrack (`ct state established,
// related`). TC-BPF hooks don't have netfilter conntrack available at all,
// so this file implements its own minimal one: a single conntrack map,
// keyed by a direction-normalized 5-tuple (so both legs of one flow hash to
// the same key), shared across every tap's attached programs (see
// main.go's MapReplacements use). A new flow that matches an explicit
// allow rule (or the implicit gateway_ip baseline, encoded as an ordinary
// entry at the front of each rules_* map by main.go) records
// itself in conntrack; any packet -- in EITHER direction -- that matches
// an existing, unexpired conntrack entry is accepted immediately, without
// re-checking the rule list. This is symmetric: it doesn't matter whether
// the VM or the remote peer sent the first packet of a flow.
//
// # Anti-spoofing
//
// Before any ACL check, enforce_vm_egress (the VM's own outbound traffic)
// requires every frame to carry the VM's own allocated MAC as its Ethernet
// source, and to be either IPv4 from its own allocated IP or ARP whose
// sender fields are its own MAC/IP (sender IP 0.0.0.0 is also allowed:
// RFC 5227 ARP probes). Every other EtherType (IPv6, 802.1Q-tagged frames)
// is dropped. This mirrors nftacl's writeAntiSpoof and is not tenant-
// configurable. The expected IP/MAC live in the per-tap spoof map;
// when it's inactive (the plugin was never told the VM's address) the
// check is skipped.
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

// Deliberately not #include <linux/in.h> etc: vmlinux.h's own generated
// struct ethhdr/iphdr/tcphdr/udphdr already cover what's needed, and mixing
// it with the real uapi headers cause redefinition errors. Same reasoning
// for hand-writing these instead of pulling in a conflicting uapi header.
#define ETH_P_IP_BE 0x0008  // ETH_P_IP (0x0800) already in network byte order
#define ETH_P_ARP_BE 0x0608 // ETH_P_ARP (0x0806) already in network byte order
#define IPPROTO_ICMP_ 1
#define IPPROTO_TCP_ 6
#define IPPROTO_UDP_ 17

#define TC_ACT_OK 0
#define TC_ACT_SHOT 2

// MAX_RULES bounds both rules_ingress/rules_egress: generous for a single
// NetworkInterface's merged SecurityGroup rules (1 gateway baseline entry
// + the rules), small enough for the verifier's bounded-loop budget with #pragma
// unroll.
#define MAX_RULES 64
// CONNTRACK_TIMEOUT_NS: how long a conntrack entry is honored without a
// fresh packet refreshing it -- an approximation of TCP/UDP idle timeouts,
// not protocol-aware (see this file's own header comment: this is the
// stateful-but-simple version; a future stateless/perf-focused
// implementation is a separate, later plugin).
#define CONNTRACK_TIMEOUT_NS (120ULL * 1000000000ULL)

// rule is one allow rule of the SNAP payload (SecurityGroup rules are
// allow-only), plus the gateway_ip baseline main.go injects as the first
// entry of each map -- see this package's README for the exact
// wire-to-map field mapping. The peer is either a CIDR or, when set_id is
// non-zero, any member of that address set (set_members).
struct rule {
	__u32 cidr_addr; // network byte order
	__u32 cidr_mask; // network byte order; 0 means "match any address"
	__u32 set_id;    // non-zero: match set_members instead of the CIDR
	__u16 port_lo;   // host byte order; ignored for ICMP
	__u16 port_hi;
	__u8 protocol;   // IPPROTO_* value; 0 means "match any protocol"
	__u8 active;     // 0 = unused slot
	__u8 _pad[2];
};

// set_member_key is one (address set, IPv4 address) membership.
struct set_member_key {
	__u32 set_id;
	__u32 addr; // network byte order
};

// spoof_cfg is the anti-spoofing input: the VM's own allocated address on
// this tap (see this file's header comment).
struct spoof_cfg {
	__u32 ip;     // network byte order
	__u8 mac[6];
	__u8 active;  // 0 = unknown address, skip the check
	__u8 _pad;
};

// arp_eth_ipv4 is an Ethernet/IPv4 ARP payload's fixed layout after the
// generic struct arphdr -- vmlinux.h only has the generic header.
struct arp_eth_ipv4 {
	__u8 sha[6];
	__u8 sip[4];
	__u8 tha[6];
	__u8 tip[4];
};

struct conntrack_key {
	__u32 ip_lo;
	__u32 ip_hi;
	__u16 port_lo;
	__u16 port_hi;
	__u8 protocol;
	__u8 _pad[3];
};

// rules_ingress holds this NetworkInterface's IngressRules (traffic
// allowed *into* the VM) -- read by enforce_vm_ingress (TC egress).
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, MAX_RULES);
	__type(key, __u32);
	__type(value, struct rule);
} rules_ingress SEC(".maps");

// rules_egress holds this NetworkInterface's EgressRules (traffic allowed
// *out of* the VM) -- read by enforce_vm_egress (TC ingress).
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, MAX_RULES);
	__type(key, __u32);
	__type(value, struct rule);
} rules_egress SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct spoof_cfg);
} spoof SEC(".maps");

// set_members holds every address set's members, shared across every
// tap's programs on this host like conntrack (main.go pins it once): a
// membership change is one map write, not a rewrite of every tap's rules.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, struct set_member_key);
	__type(value, __u8);
} set_members SEC(".maps");

// conntrack is shared across every tap's attached programs on this host
// (main.go loads/pins it once and reuses it for every subsequent tap via
// ebpf.CollectionOptions.MapReplacements) -- flows aren't tap-scoped, so
// there's no reason for each tap to keep its own copy.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct conntrack_key);
	__type(value, __u64);
} conntrack SEC(".maps");

struct flow5 {
	__u32 saddr;
	__u32 daddr;
	__u16 sport;
	__u16 dport; // rule matching always checks dport regardless of
	             // direction -- see match_ingress_rules/match_egress_rules
	             // and snap's README; sport exists only to make the
	             // conntrack key a genuine 5-tuple (see make_conntrack_key).
	__u8 protocol;
};

// parse_flow/source_ok take data/data_end rather than the skb itself: each
// program reads skb->data/data_end exactly once and passes them down, since
// letting both helpers read them independently lets clang reuse an offset
// ctx pointer, which the verifier rejects ("dereference of modified ctx
// ptr").
static __always_inline int parse_flow(void *data, void *data_end, struct flow5 *f)
{
	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return 0;
	if (eth->h_proto != ETH_P_IP_BE)
		return 0;

	struct iphdr *ip = (void *)(eth + 1);
	if ((void *)(ip + 1) > data_end)
		return 0;
	if (ip->ihl < 5)
		return 0;
	void *l4 = (void *)ip + (ip->ihl * 4);

	f->saddr = ip->saddr;
	f->daddr = ip->daddr;
	f->protocol = ip->protocol;
	f->sport = 0;
	f->dport = 0;

	if (ip->protocol == IPPROTO_TCP_) {
		struct tcphdr *tcp = l4;
		if ((void *)(tcp + 1) > data_end)
			return 0;
		f->sport = bpf_ntohs(tcp->source);
		f->dport = bpf_ntohs(tcp->dest);
	} else if (ip->protocol == IPPROTO_UDP_) {
		struct udphdr *udp = l4;
		if ((void *)(udp + 1) > data_end)
			return 0;
		f->sport = bpf_ntohs(udp->source);
		f->dport = bpf_ntohs(udp->dest);
	} else if (ip->protocol == IPPROTO_ICMP_) {
		// No ports to extract; sport/dport stay 0 and are never
		// checked for ICMP (see match_*_rules below) -- the
		// conntrack key still works fine with both left at 0, since
		// ICMP has no concept of a port to disambiguate on anyway.
	} else {
		return 0; // unsupported protocol: caller treats as default-deny
	}
	return 1;
}

// make_conntrack_key normalizes (ip, port) endpoints so both legs of the
// same flow hash to the same key, regardless of which direction's program
// observes a given packet.
static __always_inline void make_conntrack_key(struct flow5 *f, struct conntrack_key *k)
{
	__u64 a = ((__u64)f->saddr << 16) | f->sport;
	__u64 b = ((__u64)f->daddr << 16) | f->dport;
	if (a <= b) {
		k->ip_lo = f->saddr;
		k->port_lo = f->sport;
		k->ip_hi = f->daddr;
		k->port_hi = f->dport;
	} else {
		k->ip_lo = f->daddr;
		k->port_lo = f->dport;
		k->ip_hi = f->saddr;
		k->port_hi = f->sport;
	}
	k->protocol = f->protocol;
	k->_pad[0] = k->_pad[1] = k->_pad[2] = 0;
}

// peer_matches reports whether peer_addr is r's peer: in its CIDR, or a
// member of its address set.
static __always_inline int peer_matches(struct rule *r, __u32 peer_addr)
{
	if (r->set_id) {
		struct set_member_key k = {.set_id = r->set_id, .addr = peer_addr};
		return bpf_map_lookup_elem(&set_members, &k) != 0;
	}
	return (peer_addr & r->cidr_mask) == (r->cidr_addr & r->cidr_mask);
}

// match_ingress_rules/match_egress_rules are intentionally near-duplicate
// (not one function taking a map argument): a BPF map helper call must
// reference a statically-known map object at compile time, so the map
// can't be a runtime parameter. Returns 1 (some rule allows) or 0 (none
// does -- caller applies default-deny).
static __always_inline int match_ingress_rules(struct flow5 *f, __u32 peer_addr)
{
	int verdict = 0;
#pragma unroll
	for (int i = 0; i < MAX_RULES; i++) {
		__u32 idx = i;
		struct rule *r = bpf_map_lookup_elem(&rules_ingress, &idx);
		if (!r || !r->active)
			continue;
		if (r->protocol != 0 && r->protocol != f->protocol)
			continue;
		if (f->protocol != IPPROTO_ICMP_ && (f->dport < r->port_lo || f->dport > r->port_hi))
			continue;
		if (!peer_matches(r, peer_addr))
			continue;
		verdict = 1;
		break;
	}
	return verdict;
}

static __always_inline int match_egress_rules(struct flow5 *f, __u32 peer_addr)
{
	int verdict = 0;
#pragma unroll
	for (int i = 0; i < MAX_RULES; i++) {
		__u32 idx = i;
		struct rule *r = bpf_map_lookup_elem(&rules_egress, &idx);
		if (!r || !r->active)
			continue;
		if (r->protocol != 0 && r->protocol != f->protocol)
			continue;
		if (f->protocol != IPPROTO_ICMP_ && (f->dport < r->port_lo || f->dport > r->port_hi))
			continue;
		if (!peer_matches(r, peer_addr))
			continue;
		verdict = 1;
		break;
	}
	return verdict;
}

static __always_inline int mac_eq(const __u8 *a, const __u8 *b)
{
	return a[0] == b[0] && a[1] == b[1] && a[2] == b[2] &&
	       a[3] == b[3] && a[4] == b[4] && a[5] == b[5];
}

// source_ok reports whether a frame the VM sent passes anti-spoofing (see
// this file's header comment). 1 = pass (or no address configured), 0 =
// drop.
static __always_inline int source_ok(void *data, void *data_end)
{
	__u32 zero = 0;
	struct spoof_cfg *cfg = bpf_map_lookup_elem(&spoof, &zero);
	if (!cfg || !cfg->active)
		return 1;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return 0;
	if (!mac_eq(eth->h_source, cfg->mac))
		return 0;

	if (eth->h_proto == ETH_P_IP_BE) {
		struct iphdr *ip = (void *)(eth + 1);
		if ((void *)(ip + 1) > data_end)
			return 0;
		return ip->saddr == cfg->ip;
	}
	if (eth->h_proto == ETH_P_ARP_BE) {
		struct arphdr *arp = (void *)(eth + 1);
		struct arp_eth_ipv4 *body = (void *)(arp + 1);
		if ((void *)(body + 1) > data_end)
			return 0;
		if (!mac_eq(body->sha, cfg->mac))
			return 0;
		__u32 sip;
		__builtin_memcpy(&sip, body->sip, sizeof(sip));
		return sip == cfg->ip || sip == 0;
	}
	return 0;
}

// enforce_vm_egress is attached to the tap's TC ingress hook -- see this
// file's header comment for why that means "the VM's own outbound
// traffic," enforced against EgressRules (rules_egress).
SEC("tc")
int enforce_vm_egress(struct __sk_buff *skb)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	if (!source_ok(data, data_end))
		return TC_ACT_SHOT;

	struct flow5 f;
	if (!parse_flow(data, data_end, &f))
		return TC_ACT_OK; // non-IPv4/unparseable: out of scope, same as nftacl (IPv4-only)

	struct conntrack_key k;
	make_conntrack_key(&f, &k);
	__u64 now = bpf_ktime_get_ns();
	__u64 *seen = bpf_map_lookup_elem(&conntrack, &k);
	if (seen && now - *seen < CONNTRACK_TIMEOUT_NS) {
		bpf_map_update_elem(&conntrack, &k, &now, BPF_ANY);
		return TC_ACT_OK;
	}

	if (match_egress_rules(&f, f.daddr) == 1) {
		bpf_map_update_elem(&conntrack, &k, &now, BPF_ANY);
		return TC_ACT_OK;
	}
	return TC_ACT_SHOT;
}

// enforce_vm_ingress is attached to the tap's TC egress hook -- "traffic
// about to be delivered to the VM," enforced against IngressRules
// (rules_ingress).
SEC("tc")
int enforce_vm_ingress(struct __sk_buff *skb)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	struct flow5 f;
	if (!parse_flow(data, data_end, &f))
		return TC_ACT_OK;

	struct conntrack_key k;
	make_conntrack_key(&f, &k);
	__u64 now = bpf_ktime_get_ns();
	__u64 *seen = bpf_map_lookup_elem(&conntrack, &k);
	if (seen && now - *seen < CONNTRACK_TIMEOUT_NS) {
		bpf_map_update_elem(&conntrack, &k, &now, BPF_ANY);
		return TC_ACT_OK;
	}

	if (match_ingress_rules(&f, f.saddr) == 1) {
		bpf_map_update_elem(&conntrack, &k, &now, BPF_ANY);
		return TC_ACT_OK;
	}
	return TC_ACT_SHOT;
}

char _license[] SEC("license") = "GPL";
