# vrf-lite-clos

A containerlab lab verifying `examples/vnap-plugins/frr-vrf-host-route.sh`
(VNAP, the same script `playground/evpn-vxlan-clos/` uses, unmodified)
against a leaf-spine-leaf fabric running real FRR, with a per-tenant VRF
end-to-end but **no EVPN and no VXLAN anywhere** -- plain VRF-scoped
`address-family ipv4 unicast` the whole way -- the **pure L3,
IP-overlap-tolerant** deployment style's simpler alternative to EVPN+VXLAN
(see [network-deployment-guide.md](../../docs/network-deployment-guide.md)
「3.5. Pure L3デプロイの場合」).

This lab formalizes a design already verified working in an earlier
scratchpad test: `frr-vrf-host-route.sh`'s host-side behavior (`gateway_ip`
as a `/32`, `proxy_arp`, `vtysh` static-route injection inside the tenant
VRF) is **identical** whether the network side carries that route via
EVPN+VXLAN (`playground/evpn-vxlan-clos/`) or via plain VRF-scoped BGP
(here) -- the script only ever talks to FRR's RIB via `vtysh`, and which
network-side technology relays that route onward is the network team's own
choice, not the script's concern.

**FRR 10.5.1 limitation found while building this lab**: unnumbered eBGP
(`neighbor <iface> interface remote-as external`) does not establish inside
a non-default VRF instance -- this lab uses numbered (point-to-point
address) eBGP instead. `evpn-vxlan-clos`/`ipv4-unicast-clos` don't hit this
since their underlay eBGP sessions aren't VRF-scoped.

**SNAP (ACL enforcement) is out of scope here**; this lab only exercises
`-network-attach-bin` wiring, not `-security-backend-bin`.

## Topology

```
 host1 (AS 65001)                                    host2 (AS 65002)
(fake VM: 10.88.0.1)                            (fake VM: 10.88.0.2)
   |                                                      |
  tap --- vrf01c9a2a0762a (VRF)                          tap --- (same VRF)
   |                                                      |
 eth1 10.0.1.1/30                                 eth1 10.0.4.2/30
   |  numbered eBGP                                        |
 leaf1 (AS 65011) ==== spine (AS 65021) ==== leaf2 (AS 65012)
   (all VRF-scoped `address-family ipv4 unicast`, no EVPN/VXLAN)
```

The per-tenant VRF (`vrf01c9a2a0762a`) baked into the `frr.conf` files and
`topo.clab.yml`'s `exec:` blocks is this lab's own fixed test fixture --
`vrf01c9a2a0762a` is `frr-vrf-host-route.sh`'s own sha256-derived name for
`tenant_id` `"tenant-test0000000000000"`.

## VNAP plugin

`examples/vnap-plugins/frr-vrf-host-route.sh` -- see its own header comment
(including the required companion FRR config sketch, which covers both this
lab's realization and `evpn-vxlan-clos`'s) and
[specs/vnap.md](../../docs/specs/vnap.md)「参考実装」for the full design.

## Running it

```sh
playground/vrf-lite-clos/run-test.sh    # deploy + wire + ping
playground/vrf-lite-clos/cleanup.sh     # tear down
```
