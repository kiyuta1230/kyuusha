# Security-backend plugin examples

Reference implementations of the security-backend contract
(`-security-backend-bin`, a compute-agent flag) lets an operator use to
delegate ingress_rules/egress_rules enforcement to an external binary
instead of `internal/compute-agent/nftacl`'s built-in bridge-family
nftables implementation. See:

- `docs/architecture.md`「ACL強制もVNAPと同じ発想でプラガブルにすべきか」for why
  this exists as a separate contract from VNAP (`-network-attach-bin`),
  even though the exec/stdin-JSON/exit-code mechanics are identical
- `docs/specs/network.md`「セキュリティバックエンド」for the full wire
  contract (JSON payloads, exit codes, timeout, idempotency requirements)
- `internal/compute-agent/snap` for the Go-side contract these mirror

These are meant to be read and adapted to your own environment, not
deployed as-is -- see each plugin's own README/header comment for its
specific assumptions, dependencies, and trade-offs.

| Plugin | Approach | Notes |
|---|---|---|
| `ebpf-snap/` | TC-BPF (`cilium/ebpf`), stateful | Works with non-bridge tap wiring (e.g. `examples/vnap-plugins/frr-vrf-host-route.sh`'s pure-L3 setup), unlike nftacl. Tracks its own flow state (see its README) since TC hooks have no netfilter conntrack; a separate, stateless/performance-focused plugin is expected later, not this one |
