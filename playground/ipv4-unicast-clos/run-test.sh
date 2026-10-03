#!/usr/bin/env bash
# playground/ipv4-unicast-clos/run-test.sh -- deploys a leaf-spine-leaf CLOS
# lab (containerlab, topo.clab.yml) running real FRR on every node, with no
# VRF, no EVPN, no VXLAN anywhere: one ASN per hypervisor, unnumbered eBGP
# per hop, plain `address-family ipv4 unicast` the whole way, matching
# docs/network-deployment-guide.md「3.5. Pure L3デプロイの場合」's recommended reference
# config for the IP-address-unique case. Then runs kyuusha's actual
# examples/vnap-plugins/frr-ipv4-unicast.sh (the real file, not a
# reimplementation) on both "host1" and "host2" against a fake VM (a veth
# pair + netns standing in for a Firecracker tap+guest), and pings across
# the fabric.
#
# Unlike playground/evpn-vxlan-clos/ and playground/vrf-lite-clos/ (both of
# which carry a per-tenant VRF end-to-end so overlapping tenant addresses
# never collide), this lab's whole premise is that every tenant's address
# space is unique fabric-wide -- so there is nothing here enforcing tenant
# isolation at the network layer at all. See frr-ipv4-unicast.sh's own
# header comment for what backstops that assumption in a real deployment.
#
# This lab also verifies something the other two don't need: leaf1/leaf2
# each originate a default route toward their host-facing interface
# (`neighbor <iface> default-originate`), and host1 actually uses it --
# since a hypervisor with no VRF of its own has no other way to reach
# anything outside its local Subnet.
#
# Requires: containerlab (https://containerlab.dev), Docker, and passwordless
# (or interactive) sudo.
#
# Usage: playground/ipv4-unicast-clos/run-test.sh
# Leaves the lab running afterward for manual poking (docker exec
# clab-ipv4-unicast-clos-<node> vtysh); run cleanup.sh when done.
set -euo pipefail
cd "$(dirname "$0")"

REPO_ROOT="$(cd ../.. && pwd)"
LAB=ipv4-unicast-clos
SCRIPT="$REPO_ROOT/examples/vnap-plugins/frr-ipv4-unicast.sh"

dexec() { docker exec "clab-${LAB}-$1" sh -c "$2"; }

echo "== deploying containerlab topology (FRR boot + BGP convergence takes a bit) =="
sudo containerlab deploy -t topo.clab.yml --reconfigure

echo "== waiting for the underlay eBGP session (host1<->leaf1) to establish =="
for _ in $(seq 1 30); do
	dexec host1 "vtysh -c 'show bgp summary' 2>/dev/null" | grep -q "^eth1 " && break
	sleep 2
done

echo "== copying frr-ipv4-unicast.sh into host1/host2 =="
docker cp "$SCRIPT" "clab-${LAB}-host1:/frr-ipv4-unicast.sh"
docker cp "$SCRIPT" "clab-${LAB}-host2:/frr-ipv4-unicast.sh"

wire_fake_vm() {
	host="$1" tap="$2" guest_if="$3" ns="$4" ip_addr="$5"
	dexec "$host" "
		ip link add ${tap} type veth peer name ${guest_if}
		ip netns add ${ns}
		ip link set ${guest_if} netns ${ns}
		ip netns exec ${ns} ip link set lo up
		ip netns exec ${ns} ip addr add ${ip_addr}/24 dev ${guest_if}
		ip netns exec ${ns} ip link set ${guest_if} up
		echo '{\"tap_name\":\"'${tap}'\",\"ip_address\":\"'${ip_addr}'\",\"gateway_ip\":\"10.88.0.254\",\"prefix_len\":\"24\"}' | sh /frr-ipv4-unicast.sh attach
	" > /dev/null 2>&1 || true # vtysh prints a harmless missing-vtysh.conf warning to stderr on every call
}

echo "== wiring fake VM on host1 (10.88.0.1) =="
wire_fake_vm host1 tapvm1 guest1 ns1 10.88.0.1
echo "== wiring fake VM on host2 (10.88.0.2) =="
wire_fake_vm host2 tapvm2 guest2 ns2 10.88.0.2

echo "== confirming the route actually reached host2 (route propagation across 4 hops takes a few seconds) =="
route_seen=""
for _ in $(seq 1 15); do
	if dexec host2 "ip route show | grep -q 10.88.0.1"; then
		route_seen=1
		break
	fi
	sleep 2
done
[ -n "$route_seen" ] || {
	echo "FAIL: 10.88.0.1 never made it into host2's routing table -- check 'docker exec clab-${LAB}-host1 vtysh -c \"show bgp summary\"'"
	exit 1
}

echo "== pinging host1's fake VM from host2's fake VM, across the CLOS fabric (zero encapsulation) =="
if dexec host2 "ip netns exec ns2 ping -c 5 -W 2 10.88.0.1"; then
	echo "PASS: cross-host L3 (plain ipv4 unicast) reachable through the simulated CLOS fabric"
else
	echo "FAIL: no reply"
	exit 1
fi

echo "== confirming host1 actually received a default route from leaf1 via BGP =="
if dexec host1 "vtysh -c 'show ip route 0.0.0.0/0' 2>/dev/null" | grep -q 'Known via "bgp"'; then
	echo "PASS: host1 learned a BGP default route"
else
	echo "FAIL: host1 has no BGP-learned default route at all"
	exit 1
fi

echo "== dropping host1's containerlab-management default route (eth0, distance 0) so the BGP-learned one (distance 20) is the only one left in the kernel FIB -- containerlab dual-homes every node for mgmt/SSH, which a real hypervisor (fabric NIC only) would never have competing with its fabric default route =="
dexec host1 "ip route del default via 172.20.20.1 dev eth0" || true

echo "== confirming that default route is actually usable: host1 -> leaf1's own loopback (10.255.1.1), with no explicit route to it =="
if dexec host1 "ping -c 4 -W 2 10.255.1.1"; then
	echo "PASS: default-route-only destination reachable -- the Leaf-originated default route is real, not just present"
else
	echo "FAIL: no reply (default route present but not functional?)"
	exit 1
fi
