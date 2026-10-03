#!/usr/bin/env bash
# playground/evpn-vxlan-clos/run-test.sh -- see README.md in this directory
# for the topology diagram and full design rationale (including the real
# frr-vrf-host-route.sh bug this lab caught, and why it shares that script
# unmodified with playground/vrf-lite-clos/). Deploys the containerlab lab,
# runs examples/vnap-plugins/frr-vrf-host-route.sh against a fake VM on both
# "host1" and "host2", and pings across the fabric.
#
# Requires: containerlab (https://containerlab.dev), Docker, and passwordless
# (or interactive) sudo.
#
# Usage: playground/evpn-vxlan-clos/run-test.sh
# Leaves the lab running afterward for manual poking (docker exec
# clab-evpn-vxlan-clos-<node> vtysh); run cleanup.sh when done.
set -euo pipefail
cd "$(dirname "$0")"

REPO_ROOT="$(cd ../.. && pwd)"
LAB=evpn-vxlan-clos
TENANT_ID="tenant-test0000000000000"
VRF="vrf01c9a2a0762a" # must match: printf '%s' "$TENANT_ID" | sha256sum | cut -c1-12, prefixed "vrf"
SCRIPT="$REPO_ROOT/examples/vnap-plugins/frr-vrf-host-route.sh"

dexec() { docker exec "clab-${LAB}-$1" sh -c "$2"; }

echo "== deploying containerlab topology (FRR boot + BGP convergence takes a bit) =="
sudo containerlab deploy -t topo.clab.yml --reconfigure

echo "== waiting for the underlay eBGP session (host1<->leaf1) to establish =="
for _ in $(seq 1 30); do
	dexec host1 "vtysh -c 'show bgp summary' 2>/dev/null" | grep -q "^eth1 " && break
	sleep 2
done

echo "== copying frr-vrf-host-route.sh into host1/host2 =="
docker cp "$SCRIPT" "clab-${LAB}-host1:/frr-vrf-host-route.sh"
docker cp "$SCRIPT" "clab-${LAB}-host2:/frr-vrf-host-route.sh"

wire_fake_vm() {
	host="$1" tap="$2" guest_if="$3" ns="$4" ip_addr="$5"
	dexec "$host" "
		ip link add ${tap} type veth peer name ${guest_if}
		ip netns add ${ns}
		ip link set ${guest_if} netns ${ns}
		ip netns exec ${ns} ip link set lo up
		ip netns exec ${ns} ip addr add ${ip_addr}/24 dev ${guest_if}
		ip netns exec ${ns} ip link set ${guest_if} up
		echo '{\"tap_name\":\"'${tap}'\",\"tenant_id\":\"'${TENANT_ID}'\",\"ip_address\":\"'${ip_addr}'\",\"gateway_ip\":\"10.88.0.254\"}' | sh /frr-vrf-host-route.sh attach
	" > /dev/null 2>&1 || true # vtysh prints a harmless missing-vtysh.conf warning to stderr on every call
}

echo "== wiring fake VM on host1 (10.88.0.1) =="
wire_fake_vm host1 tapvm1 guest1 ns1 10.88.0.1
echo "== wiring fake VM on host2 (10.88.0.2) =="
wire_fake_vm host2 tapvm2 guest2 ns2 10.88.0.2

echo "== confirming the EVPN Type-5 route actually reached host2's VRF table (route propagation across 4 hops takes a few seconds) =="
route_seen=""
for _ in $(seq 1 15); do
	if dexec host2 "ip route show table 1000 | grep -q 10.88.0.1"; then
		route_seen=1
		break
	fi
	sleep 2
done
[ -n "$route_seen" ] || {
	echo "FAIL: 10.88.0.1 never made it into host2's vrf table -- check 'docker exec clab-${LAB}-host1 vtysh -c \"show bgp l2vpn evpn\"'"
	exit 1
}

echo "== pinging host1's fake VM from host2's fake VM, across the CLOS fabric (real VXLAN encap) =="
if dexec host2 "ip netns exec ns2 ping -c 5 -W 2 10.88.0.1"; then
	echo "PASS: cross-host L3 (EVPN Type-5) reachable through the simulated CLOS fabric"
else
	echo "FAIL: no reply"
	exit 1
fi
