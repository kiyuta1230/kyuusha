# vlan-clos

A containerlab lab verifying `examples/vnap-plugins/vlan-trunk.sh` (VNAP, VM
Network Attach Protocol) -- the **L2 VLAN trunk** reference implementation --
against a realistic leaf-spine-leaf switched fabric, independent of whatever
quirks any one Docker network happens to have.

This is not a test of kyuusha's own services (no api-gateway/compute/network,
no real Firecracker VM) -- it exists purely to validate the VNAP plugin's
actual cross-host L2 wiring. **SNAP (ACL enforcement) is out of scope here**;
this lab only exercises `-network-attach-bin` wiring, not
`-security-backend-bin`.

## Topology

```
 host1                                                      host2
(fake VM: 10.99.0.1)                                (fake VM: 10.99.0.2)
   |                                                         |
  tap (vlan-trunk.sh: tap -> bridge -> 802.1Q uplink)       tap
   |                                                         |
 eth1                                                      eth1
   |                                                         |
 leaf1 ===== (br0, VLAN-aware, trunk vid 4) ===== spine ===== (same) ===== leaf2
```

`leaf1` also carries the Subnet's gateway: an SVI on VLAN 4 (`10.99.0.254`)
plus an address beyond it (`192.0.2.1`), standing in for the leaf's SVI in the
tenant's VRF. `vlan-trunk.sh` puts no address on the hypervisor bridges -- the
gateway lives only in the fabric -- and the test checks exactly that: no host
bridge carries `gateway_ip`, ARP for the gateway is answered by the SVI alone,
and both fake VMs reach `192.0.2.1` through it.

`leaf1`/`spine`/`leaf2` are plain Linux VLAN-aware bridges (`br0`,
`vlan_filtering 1`) standing in for real switches, trunking VLAN ID 4 across
every link -- the thing a real ToR's trunk port configuration would do (see
[network-deployment-guide.md](../../docs/network-deployment-guide.md)
「1. VLANプール設計」). `host1`/`host2` run `vlan-trunk.sh attach` against a
veth+netns pair standing in for a Firecracker tap+guest, which creates an
802.1Q subinterface on `eth1` (the uplink, passed via `VNAP_UPLINK_IFACE`) and
bridges the tap to it.

## VNAP plugin

`examples/vnap-plugins/vlan-trunk.sh` -- see its own header comment and
[specs/vnap.md](../../docs/specs/vnap.md)「参考実装」for the full design.

## Running it

```sh
playground/vlan-clos/run-test.sh    # deploy + wire + ping
playground/vlan-clos/cleanup.sh     # tear down
```
