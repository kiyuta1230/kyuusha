# ebpf-secacl

A reference implementation of kyuusha's security-backend plugin contract
(`internal/compute-agent/secacl`), enforcing `ingress_rules`/`egress_rules`
natively in eBPF (TC-BPF, attached directly to the VM's tap device) instead
of the default `nftacl` (bridge-family nftables). See
`../README.md`/`docs/specs/network.md`「セキュリティバックエンド」 for the
contract itself.

**This is the stateful version.** It tracks established flows itself in a
BPF map (see `bpf/secacl.c`'s own doc comment), since TC-BPF hooks have no
access to netfilter's own conntrack the way `nftacl`'s bridge-family
`ct state established,related` does. A hypothetical future, separate
plugin is expected to trade this away for raw throughput -- this one
favors correctness/parity with `nftacl`'s behavior.

## Why eBPF, and why this is *not* a drop-in replacement for nftacl

`nftacl` needs the tap to be a Linux bridge port (that's what gives it
access to `ct state`). TC-BPF attaches directly to the tap device itself,
so it works regardless of what (if anything) the tap is plugged into
downstream -- including non-bridge VNAP wiring like
`examples/vnap-plugins/frr-type5.sh`'s EVPN Type-5 setup, where there is no
shared bridge at all.

## Design notes

- **Direction naming** mirrors `nftacl` exactly (same tap-centric
  inversion of the ingress_rules/egress_rules naming -- see both files'
  header comments): `enforce_vm_egress` (attached to TC **ingress**,
  i.e. the VM's own outbound traffic) enforces `EgressRules`;
  `enforce_vm_ingress` (attached to TC **egress**, traffic delivered to
  the VM) enforces `IngressRules`.
- **Statefulness**: a single BPF map (`conntrack`, `BPF_MAP_TYPE_LRU_HASH`)
  keyed by a direction-normalized 5-tuple, **shared across every tap** on
  the host (not per-tap) -- a new flow that matches an allow rule (explicit
  or the implicit own-Subnet-CIDR/gateway_ip baseline) records itself
  there; any packet in *either* direction matching an existing, unexpired
  entry is accepted immediately without re-checking the rule list. This is
  symmetric: it works whether the VM or the remote peer sends the first
  packet.
  - **Existing flows aren't retroactively re-evaluated.** Same as
    `nftacl`'s own `ct state` table: changing the rule set (a new
    `UpdateFirewallRules` call) doesn't tear down a flow that was already
    allowed and is still within the conntrack timeout. This was confirmed
    hands-on, not assumed -- see the veth-pair test log this plugin was
    verified with.
  - Timeout is a single fixed constant (`CONNTRACK_TIMEOUT_NS` in
    `bpf/secacl.c`), not protocol-aware. Adjust if 120s doesn't fit your
    workload.
- **Rules**: up to 64 entries per direction per tap (`MAX_RULES`), first
  match wins, exactly like `nftacl`. The Subnet CIDR and gateway_ip
  baseline are injected as ordinary entries at the front of each list by
  `main.go`, reusing the same generic matching code as operator-specified
  rules (no special-cased C logic for the baseline).
- **Attachment**: TCX (`link.AttachTCX`), the modern qdisc-free kernel
  attach point -- no `tc qdisc add ... clsact` needed, unlike classic
  tc-BPF filters.
- **Re-apply** (a later `UpdateFirewallRules`-triggered `attach` call for
  the same tap): only rewrites the rule maps' contents. The already-
  attached programs keep running throughout, so there's no enforcement gap
  during an update.
- **IPv4 only**, matching `nftacl`'s own scope. Non-IPv4 traffic is passed
  through unfiltered (`TC_ACT_OK`) rather than blocked, same reasoning as
  `nftacl` never touching anything outside `ip`/`ip6` families it doesn't
  understand.

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
go generate ./...   # only if you changed bpf/secacl.c or regenerated vmlinux.h
go build -o ebpf-secacl .
```

## Use

Point compute-agent at the built binary:

```sh
compute-agent ... -security-backend-bin=/path/to/ebpf-secacl
```

## Verified

Hands-on against a real veth pair + network namespace (simulating a tap
connected to a VM), not just inspected: baseline (own-Subnet-CIDR/
gateway_ip) traffic always passes; an explicit `allow` rule for a
peer/protocol/port passes; a fresh (never-before-seen) flow with no
matching rule is dropped; the same flow with an explicit `deny` rule is
dropped; and -- the core statefulness claim -- a VM-initiated flow allowed
only by an `egress_rules` entry gets its *reply* traffic through with an
**empty** `ingress_rules` list, purely via the conntrack map. `detach`
was confirmed to remove the TC attachment and this tap's pinned maps
(`bpftool net show dev <tap>` shows nothing left) while leaving the shared
`conntrack` map alone, and is safe to call twice.
