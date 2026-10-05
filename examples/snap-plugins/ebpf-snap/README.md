# ebpf-snap

A reference implementation of kyuusha's security-backend plugin contract
(`internal/compute-agent/snap`), enforcing a NetworkInterface's
SecurityGroup rules natively in eBPF (TC-BPF, attached directly to the VM's tap device) instead
of the default `nftacl` (bridge-family nftables). See
`../README.md`/`docs/specs/snap.md` for the contract itself (`attach`,
`detach`, `update_sets`).

**This is the stateful version.** It tracks established flows itself in a
BPF map (see `bpf/snap.c`'s own doc comment), since TC-BPF hooks have no
access to netfilter's own conntrack the way `nftacl`'s bridge-family
`ct state established,related` does. A hypothetical future, separate
plugin is expected to trade this away for raw throughput -- this one
favors correctness/parity with `nftacl`'s behavior.

## Why eBPF, and why this is *not* a drop-in replacement for nftacl

`nftacl` needs the tap to be a Linux bridge port (that's what gives it
access to `ct state`). TC-BPF attaches directly to the tap device itself,
so it works regardless of what (if anything) the tap is plugged into
downstream -- including non-bridge VNAP wiring like
`examples/vnap-plugins/frr-vrf-host-route.sh`'s pure-L3 setup, where there
is no shared bridge at all.

## Design notes

- **Direction naming** mirrors `nftacl` exactly (same tap-centric
  inversion of the ingress_rules/egress_rules naming -- see both files'
  header comments): `enforce_vm_egress` (attached to TC **ingress**,
  i.e. the VM's own outbound traffic) enforces `egress_rules`;
  `enforce_vm_ingress` (attached to TC **egress**, traffic delivered to
  the VM) enforces `ingress_rules`.
- **Statefulness**: a single BPF map (`conntrack`, `BPF_MAP_TYPE_LRU_HASH`)
  keyed by a direction-normalized 5-tuple, **shared across every tap** on
  the host (not per-tap) -- a new flow that matches an allow rule (a
  SecurityGroup rule, or the implicit gateway_ip baseline) records itself
  there; any packet in *either* direction matching an existing, unexpired
  entry is accepted immediately without re-checking the rule list. This is
  symmetric: it works whether the VM or the remote peer sends the first
  packet.
  - **Existing flows aren't retroactively re-evaluated.** Same as
    `nftacl`'s own `ct state` table: changing the rule set (or removing
    a peer from an address set) doesn't tear down a flow that was already
    allowed and is still within the conntrack timeout. This was confirmed
    hands-on, not assumed -- see the veth-pair test log this plugin was
    verified with.
  - Timeout is a single fixed constant (`CONNTRACK_TIMEOUT_NS` in
    `bpf/snap.c`), not protocol-aware. Adjust if 120s doesn't fit your
    workload.
- **Rules**: allow-only, up to 64 entries per direction per tap
  (`MAX_RULES`); any match allows, nothing matching drops. The gateway_ip
  baseline is injected as an ordinary entry at the front of each list by
  `main.go`, reusing the same generic matching code as the SecurityGroup
  rules (no special-cased C logic for the baseline). A rule's peer is a
  CIDR or, when `set_id` is non-zero, any member of that address set.
- **Address sets** (`sg:<id>`, `network:<id>`): one host-wide
  `BPF_MAP_TYPE_HASH` (`set_members`, pinned next to `conntrack`), keyed
  by (set id, IPv4 address), where the set id is FNV-1a of the set's name
  (0 reserved for "a CIDR rule"; a collision would merge two sets -- a
  reference implementation's trade-off). `attach` writes the full copies
  it's handed; `update_sets` applies full copies (drop the set's keys,
  write the members) and add/remove deltas as map writes, touching no
  tap's rules. Version checks are compute-agent's, not this plugin's.
  Members of sets no tap references any more aren't garbage collected.
- **Attachment**: TCX (`link.AttachTCX`), the modern qdisc-free kernel
  attach point -- no `tc qdisc add ... clsact` needed, unlike classic
  tc-BPF filters.
- **Re-apply** (a later `update_acl`-triggered `attach` call for the
  same tap): only rewrites the rule maps' contents. The already-
  attached programs keep running throughout, so there's no enforcement gap
  during an update.
- **Anti-spoofing** (same rules as `nftacl`'s, see `docs/specs/snap.md`
  「アンチスプーフィング」): before any ACL check, `enforce_vm_egress`
  drops a frame from the VM unless its Ethernet source is the VM's own
  `mac_address` and it is either IPv4 from its own `ip_address`, or ARP
  whose sender MAC/IP are its own (sender IP `0.0.0.0` also allowed, for
  RFC 5227 probes). Every other EtherType the VM sends (IPv6, 802.1Q
  tagged frames) is dropped. The expected address lives in the per-tap
  `spoof` map; an `attach` with `ip_address` or `mac_address` empty leaves
  it as it is (a re-apply must never switch the check off).
- **ACL rules are IPv4 only**: IPv6 CIDR rules are skipped (anti-spoofing
  drops every non-IPv4 frame the VM sends anyway). Apart from
  the anti-spoofing check above, non-IPv4 traffic (ARP) is passed through
  the rule matching unfiltered (`TC_ACT_OK`).
- **Upgrading**: a tap attached by an older build has no pinned `spoof`
  map, so a re-apply for it fails with a hint to detach and re-attach.

## Requirements

- **To regenerate** (`go generate`): `clang`, `bpftool` (for
  `bpf/vmlinux.h`, already checked into this repo -- regenerate only if
  targeting a materially different kernel), `libbpf-dev` headers. Not
  needed at runtime -- `go build` embeds the compiled bytecode.
- **At runtime**: a kernel with TCX support (6.6+), `/sys/fs/bpf` mounted,
  `CAP_BPF`+`CAP_NET_ADMIN` (compute-agent's containers already run
  `privileged: true`, which covers this).

## Build

```sh
go generate ./...   # only if you changed bpf/snap.c or regenerated vmlinux.h
go build -o ebpf-snap .
```

## Use

Point compute-agent at the built binary:

```sh
compute-agent ... -security-backend-bin=/path/to/ebpf-snap
```

## Verified

Hands-on against a real veth pair + network namespace (simulating a tap
connected to a VM), not just inspected: gateway_ip traffic always passes;
an `allow` rule for a peer/protocol/port passes; a fresh (never-before-seen)
flow with no matching rule is dropped; and -- the core statefulness claim
-- a VM-initiated flow allowed only by an `egress_rules` entry gets its
*reply* traffic through with an **empty** `ingress_rules` list, purely via
the conntrack map. `detach`
was confirmed to remove the TC attachment and this tap's pinned maps
(`bpftool net show dev <tap>` shows nothing left) while leaving the shared
`conntrack` map alone, and is safe to call twice.

Anti-spoofing has an automated version of the same kind of check,
`antispoof_test.go` (root only, skipped otherwise):

```sh
go test -c -o /tmp/ebpf-snap.test . && sudo /tmp/ebpf-snap.test -test.v
```

It confirms legitimate traffic passes, and that frames with a spoofed
source IP, a spoofed source MAC, or a spoofed ARP sender IP never get past
TC ingress (counted with nftables input-hook counters, which run after
TCX) -- including after a re-attach that carries no address.
`sets_test.go` does the same for address sets: a host address reaches the
VM through a set-referencing ingress rule only while `update_sets` has it
in the set (add, remove, full copy).
