# ipv4-unicast-clos

A containerlab lab verifying `examples/vnap-plugins/frr-ipv4-unicast.sh`
(VNAP) against a leaf-spine-leaf fabric running real FRR, with no VRF, no
EVPN, no VXLAN anywhere -- plain BGP `address-family ipv4 unicast` the whole
way, one ASN per hypervisor, unnumbered eBGP per hop -- the **pure L3,
IP-unique** deployment style (see
[network-deployment-guide.md](../../docs/network-deployment-guide.md)
「3.5. Pure L3デプロイの場合」).

Unlike `playground/evpn-vxlan-clos/` and `playground/vrf-lite-clos/` (both of
which carry a per-tenant VRF end-to-end so overlapping tenant addresses never
collide), this lab's whole premise is that every tenant's address space is
**unique fabric-wide** -- so there is nothing here enforcing tenant isolation
at the network layer at all. Tenant isolation is delegated elsewhere
(kyuusha's own IPAM uniqueness guarantee, and the tap-side default-deny ACL
baseline -- see `frr-ipv4-unicast.sh`'s own header comment for what backstops
this assumption in a real deployment). **SNAP (ACL enforcement) is out of
scope in this lab itself**; it only exercises `-network-attach-bin` wiring,
not `-security-backend-bin`.

This lab also verifies something the other two don't need: `leaf1`/`leaf2`
each originate a default route toward their host-facing interface
(`neighbor <iface> default-originate`), and `host1` actually uses it -- since
a hypervisor with no VRF of its own has no other way to reach anything
outside its local Subnet (the shared NAT gateway, DNS resolver, etc.).

## Topology

```
 host1 (genuine L3 gateway for 10.88.0.0/24)         host2
(fake VM: 10.88.0.1)                          (fake VM: 10.88.0.2)
   |                                                   |
  tap (gateway_ip assigned with real /24, proxy_arp)   tap
   |                                                   |
 eth1                                                eth1
   |  unnumbered eBGP (ASN per host)                   |
 leaf1 ======= spine ======= leaf2
   |  default-originate                   default-originate |
   (host-facing interface)                (host-facing interface)
```

## VNAP plugin

`examples/vnap-plugins/frr-ipv4-unicast.sh` -- see its own header comment
(including the required companion FRR config sketch, and the proxy-ARP
trade-off that comes with a real-prefix gateway) and
[specs/vnap.md](../../docs/specs/vnap.md)「参考実装」for the full design.

## Running it

```sh
playground/ipv4-unicast-clos/run-test.sh    # deploy + wire + ping + default-route check
playground/ipv4-unicast-clos/cleanup.sh     # tear down
```
