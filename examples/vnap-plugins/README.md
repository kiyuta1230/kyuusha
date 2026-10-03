# VNAP plugin examples

Reference implementations of VNAP (VM Network Attach Protocol), the
contract `-network-attach-bin` (a compute-agent flag) lets an operator use
to delegate the local tap-to-switch attach/detach step to an external
binary instead of `internal/compute-agent/netsetup`'s built-in Linux
bridge implementation. See:

- `docs/architecture.md`「VMのネットワーク接続をCNIのようにプラガブルにすべきか」
  for why this exists and why it deliberately isn't CNI-compatible
- `docs/specs/vnap.md` for the full wire contract (JSON payloads, exit
  codes, timeout, idempotency requirements)
- `docs/network-deployment-guide.md` for the physical-network-side
  prerequisites these examples assume

These are meant to be read and adapted to your own environment, not
deployed as-is -- see each script's own header comment for its specific
assumptions and dependencies.

| Script | Deployment style |
|---|---|
| `vlan-trunk.sh` | VLAN trunk (Type-2): the built-in Linux-bridge implementation plus a real 802.1Q-tagged uplink NIC trunk, so two VMs on the same Subnet actually reach each other across Hypervisors |
| `frr-ipv4-unicast.sh` | Pure L3, IP-unique: no VRF anywhere -- every VM's `/32` goes straight into FRR's one shared routing table, relayed by plain BGP `address-family ipv4 unicast`. Only sound when every tenant's address space is guaranteed unique fabric-wide (see the script's own header comment for what that assumption relies on) |
| `frr-vrf-host-route.sh` | Pure L3, IP-overlap tolerant: every VM's `/32` goes into a per-tenant VRF instead, so overlapping tenant addresses never collide. The network side can relay that VRF's routes via either a real BGP EVPN Type-5 + VXLAN fabric (`playground/evpn-vxlan-clos/`) or plain VRF-scoped `ipv4 unicast` eBGP with no EVPN/VXLAN at all (`playground/vrf-lite-clos/`) -- this script's own attach/detach logic is identical either way, since it only ever talks to FRR's RIB, never to the BGP/EVPN config itself |
