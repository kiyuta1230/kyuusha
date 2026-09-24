# VNAP plugin examples

Reference implementations of VNAP (VM Network Attach Protocol), the
contract `-network-attach-bin` (a compute-agent flag) lets an operator use
to delegate the local tap-to-switch attach/detach step to an external
binary instead of `internal/compute-agent/netsetup`'s built-in Linux
bridge implementation. See:

- `docs/architecture.md`「VMのネットワーク接続をCNIのようにプラガブルにすべきか」
  for why this exists and why it deliberately isn't CNI-compatible
- `docs/specs/network.md`「VNAP（ローカルなtap配線プラグイン契約）」for the full
  wire contract (JSON payloads, exit codes, timeout, idempotency requirements)
- `docs/network-deployment-guide.md` for the physical-network-side
  prerequisites these examples assume

These are meant to be read and adapted to your own environment, not
deployed as-is -- see each script's own header comment for its specific
assumptions and dependencies.

| Script | Deployment style |
|---|---|
| `frr-type5.sh` | EVPN Type-5 (pure L3): no shared per-Subnet bridge, each VM's tap gets its own point-to-point-shaped `/32` presence, advertised via FRR |
