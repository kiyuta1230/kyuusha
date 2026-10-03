# evpn-vxlan-clos

A containerlab lab verifying `examples/vnap-plugins/frr-vrf-host-route.sh`
(VNAP) against a leaf-spine-leaf fabric running real FRR, with a genuine BGP
EVPN Type-5 (IP-VRF-to-IP-VRF, VXLAN-encapsulated) control and data plane --
the **pure L3, IP-overlap-tolerant** deployment style (see
[network-deployment-guide.md](../../docs/network-deployment-guide.md)
「3.5. Pure L3デプロイの場合」).

**This lab is what caught a real bug** in `frr-vrf-host-route.sh`, back when
it was still named `frr-type5.sh`: the script never enslaved the tap into the
tenant VRF, so its injected static route silently never installed into the
kernel/RIB (see the script's own header comment and
[release-notes.md](../../docs/release-notes.md)). Re-run this after any
future change to that script.

`frr-vrf-host-route.sh` is also what `playground/vrf-lite-clos/` uses, with
**identical host-side behavior** -- only this lab's leaf/spine side (EVPN AFI
+ a real VXLAN VTEP) differs from vrf-lite-clos's (plain VRF-scoped
`ipv4 unicast`, no EVPN/VXLAN at all). The script itself only ever talks to
FRR's RIB via `vtysh`; which network-side technology carries that route
onward is the network team's own choice, not the script's concern -- this is
the whole reason these two labs can share one script.

**SNAP (ACL enforcement) is out of scope here**; this lab only exercises
`-network-attach-bin` wiring, not `-security-backend-bin`.

## Topology

```
 host1 (VTEP 10.255.0.1)                           host2 (VTEP 10.255.0.2)
(fake VM: 10.88.0.1)                               (fake VM: 10.88.0.2)
   |                                                        |
  tap --- vrf01c9a2a0762a --- br104000 --- vxlan104000       (same, mirrored)
   |        (VRF)             (SVI)        (L3VNI 104000)   |
 eth1                                                      eth1
   |  unnumbered eBGP (ASN per host)      unnumbered eBGP     |
 leaf1 ======= spine ======= leaf2
   (EVPN AFI, advertise-all-vni, transit ASN)
```

The per-tenant VRF (`vrf01c9a2a0762a`), its L3VNI (`104000`), and the
route-target (`999:100`) baked into the `frr.conf` files and
`topo.clab.yml`'s `exec:` blocks are this lab's own fixed test fixture --
`vrf01c9a2a0762a` is `frr-vrf-host-route.sh`'s own sha256-derived name for
`tenant_id` `"tenant-test0000000000000"`.

## VNAP plugin

`examples/vnap-plugins/frr-vrf-host-route.sh` -- see its own header comment
(including the required companion FRR config sketch) and
[specs/vnap.md](../../docs/specs/vnap.md)「参考実装」for the full design.

## Running it

```sh
playground/evpn-vxlan-clos/run-test.sh    # deploy + wire + ping
playground/evpn-vxlan-clos/cleanup.sh     # tear down
```
