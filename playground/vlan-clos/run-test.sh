#!/usr/bin/env bash
# playground/vlan-clos/run-test.sh -- see README.md in this directory for
# the topology diagram and full design rationale. Deploys the containerlab
# lab, runs examples/vnap-plugins/vlan-trunk.sh against a fake VM on both
# "host1" and "host2", and pings across the fabric -- VM to VM, and VM to
# outside the Subnet through the fabric's gateway (an SVI on leaf1), which
# must be the only thing answering ARP for gateway_ip.
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
GATEWAY=10.99.0.254
OUTSIDE=192.0.2.1 # an address beyond the gateway, on leaf1
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

echo "== giving leaf1 the Subnet's gateway: an SVI on VLAN ${VLAN_ID} (${GATEWAY}), plus ${OUTSIDE} beyond it =="
dexec leaf1 "
	bridge vlan add dev br0 vid ${VLAN_ID} self
	ip link add link br0 name br0.${VLAN_ID} type vlan id ${VLAN_ID}
	ip addr add ${GATEWAY}/24 dev br0.${VLAN_ID}
	ip link set br0.${VLAN_ID} up
	ip addr add ${OUTSIDE}/32 dev lo
	ip link set lo up
"

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
		ip netns exec ${ns} ip route add default via ${GATEWAY}
		echo '{\"tap_name\":\"'${vm_if}'\",\"subnet_values\":{\"vlan_id\":${VLAN_ID}},\"gateway_ip\":\"${GATEWAY}\",\"prefix_len\":24}' | VNAP_UPLINK_IFACE=eth1 sh /vlan-trunk.sh attach
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

for host in host1 host2; do
	if dexec "$host" "ip -4 addr show dev kbr${VLAN_ID}" | grep -q inet; then
		echo "FAIL: $host's kbr${VLAN_ID} carries an IPv4 address; gateway_ip must live only on the fabric"
		exit 1
	fi
done
echo "PASS: no hypervisor bridge carries gateway_ip"

echo "== ARP for the gateway must get exactly one answer (leaf1's SVI) =="
macs="$(dexec host2 "ip netns exec ns2 arping -c 3 -w 3 -I guest2 ${GATEWAY}" | grep -oiE '([0-9a-f]{2}:){5}[0-9a-f]{2}' | sort -u)"
if [ "$(printf '%s\n' "$macs" | grep -c .)" -ne 1 ]; then
	echo "FAIL: gateway ARP answered by: ${macs:-nobody}"
	exit 1
fi
echo "PASS: gateway ARP answered only by $macs"

for pair in host1:ns1 host2:ns2; do
	host="${pair%%:*}" ns="${pair##*:}"
	echo "== ${host}'s fake VM -> ${OUTSIDE}, outside the Subnet via the fabric gateway =="
	if dexec "$host" "ip netns exec ${ns} ping -c 2 -W 2 ${OUTSIDE}"; then
		echo "PASS: ${host} reaches beyond the Subnet through the fabric gateway"
	else
		echo "FAIL: ${host} cannot reach ${OUTSIDE}"
		exit 1
	fi
done
