#!/usr/bin/env bash
# playground/vlan-clos/run-test.sh -- deploys a leaf-spine-leaf CLOS
# lab (containerlab, topo.clab.yml) with real VLAN-aware (802.1Q
# trunk-capable) Linux-bridge switches, then runs kyuusha's actual
# examples/vnap-plugins/vlan-trunk.sh (the real file, not a
# reimplementation) on both "host1" and "host2" nodes against a fake VM (a
# veth pair + a network namespace standing in for a Firecracker tap+guest --
# the script only cares about tap_name as a string to enslave into a
# bridge, so it can't tell the difference), and pings across the fabric.
#
# This is NOT playground/scenario.sh's kind of test (no real kyuusha
# services, no real Firecracker VM) -- it exists purely to validate a VNAP
# reference plugin's actual cross-host L2 wiring against a topology shaped
# like a real switched fabric, independent of whatever quirks any one
# Docker network happens to have. Reuse it (with a different plugin script
# and payload) for validating a future VNAP reference implementation too.
#
# Requires: containerlab (https://containerlab.dev), Docker, and passwordless
# (or interactive) sudo -- containerlab itself needs root to wire veth links
# between container network namespaces.
#
# Usage: playground/vlan-clos/run-test.sh
# Leaves the lab running afterward for manual poking (docker exec
# clab-vlan-clos-<node> sh); run cleanup.sh when done.
set -euo pipefail
cd "$(dirname "$0")"

REPO_ROOT="$(cd ../.. && pwd)"
LAB=vlan-clos
VLAN_ID=4
SCRIPT="$REPO_ROOT/examples/vnap-plugins/vlan-trunk.sh"

dexec() { docker exec "clab-${LAB}-$1" sh -c "$2"; }

echo "== deploying containerlab topology =="
sudo containerlab deploy -t topo.clab.yml --reconfigure

echo "== configuring VLAN-aware bridges on leaf1/leaf2/spine =="
for sw in leaf1 leaf2 spine; do
	dexec "$sw" "
		ip link add br0 type bridge vlan_filtering 1
		ip link set br0 up
		for p in eth1 eth2; do
			ip link set \$p master br0
			ip link set \$p up
			bridge vlan del dev \$p vid 1 2>/dev/null || true
			bridge vlan add dev \$p vid ${VLAN_ID}
		done
	"
done

echo "== copying vlan-trunk.sh into host1/host2 =="
docker cp "$SCRIPT" "clab-${LAB}-host1:/vlan-trunk.sh"
docker cp "$SCRIPT" "clab-${LAB}-host2:/vlan-trunk.sh"

wire_fake_vm() {
	host="$1" vm_if="$2" guest_if="$3" ns="$4" ip_addr="$5"
	dexec "$host" "
		ip link set eth1 up
		ip link add ${vm_if} type veth peer name ${guest_if}
		ip netns add ${ns}
		ip link set ${guest_if} netns ${ns}
		ip netns exec ${ns} ip link set lo up
		ip netns exec ${ns} ip addr add ${ip_addr}/24 dev ${guest_if}
		ip netns exec ${ns} ip link set ${guest_if} up
		echo '{\"tap_name\":\"'${vm_if}'\",\"vlan_id\":\"${VLAN_ID}\"}' | VNAP_UPLINK_IFACE=eth1 sh /vlan-trunk.sh attach
		ip link set ${vm_if} up
	"
}

echo "== wiring fake VM on host1 (10.99.0.1) =="
wire_fake_vm host1 vethvm1 guest1 ns1 10.99.0.1
echo "== wiring fake VM on host2 (10.99.0.2) =="
wire_fake_vm host2 vethvm2 guest2 ns2 10.99.0.2

echo "== pinging host2's fake VM from host1's fake VM, across the CLOS fabric =="
if dexec host1 "ip netns exec ns1 ping -c 4 -W 2 10.99.0.2"; then
	echo "PASS: cross-host L2 reachable through the simulated CLOS fabric"
else
	echo "FAIL: no reply"
	exit 1
fi
