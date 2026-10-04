#!/bin/sh
# vlan-trunk.sh -- a reference VNAP (VM Network Attach Protocol; see
# docs/architecture.md「VMのネットワーク接続をCNIのようにプラガブルにすべきか」
# and docs/specs/vnap.md) plugin for a VLAN-trunk (Type-2) deployment (see
# docs/network-deployment-guide.md's default topology).
#
# This is the built-in Linux-bridge implementation
# (internal/compute-agent/netsetup) plus the one piece it deliberately
# leaves out: it never touches a physical uplink NIC, so two VMs on the
# same Subnet but different Hypervisors can't reach each other (see
# netsetup's own package doc comment). This script does the same local
# bridge-per-vlan_id wiring, and additionally trunks that vlan_id onto a
# real uplink NIC with an 802.1Q VLAN sub-interface, so the switch side
# (a core switch's SVI, or a CLOS fabric's chosen gateway switch -- either
# way, the network team's own responsibility, unchanged) actually sees the
# tagged traffic. Per VM, "attach":
#
#   1. ensures a shared Linux bridge for this vlan_id (kbr<vlan_id>, same
#      naming netsetup's own bridgeName() uses -- harmless to share the
#      name since netsetup's own bridge code never runs on this path).
#      Unlike the built-in path, the bridge gets NO address: the Subnet's
#      gateway_ip belongs to the fabric (the leaf/ToR SVI in the VRF that
#      isolates this tenant -- docs/network-deployment-guide.md's default
#      topology). Putting it on every hypervisor's bridge too would have
#      every host answer ARP for the same IP with its own MAC on the same
#      VLAN (contending with each other and with the SVI), and a host that
#      wins would route the guest's traffic itself, around the fabric's
#      VRF. This script is a pure L2 extension.
#   2. ensures a VLAN sub-interface of $VNAP_UPLINK_IFACE for this
#      vlan_id exists and is a port on that bridge -- this is the actual
#      cross-host L2 extension the built-in path is missing
#   3. attaches this VM's own tap to the same bridge
#
# Unlike frr-vrf-host-route.sh/frr-ipv4-unicast.sh, nothing here is FRR/BGP-specific or otherwise
# protocol-configuration-heavy -- the switch side needs an ordinary trunk
# port allowing this AZ's VLAN range (docs/network-deployment-guide.md
# already assumes this for the default topology) and, as each Subnet's
# gateway_ip, a plain SVI (core/ToR) or a designated gateway switch (CLOS)
# -- required, since nothing on the hypervisor answers for gateway_ip.
#
# Requires: a real iproute2 `ip` (not busybox's -- needs `type vlan`),
# `jq`, and the environment variable VNAP_UPLINK_IFACE set to the name of
# this host's VLAN-trunked uplink NIC (a physical interface, or a bond) --
# there's no per-request field for this in the VNAP payload since it's a
# fixed, host-level fact, not something that varies per attach/detach call.
#
# This is a REFERENCE implementation, meant to be read and adapted, not
# deployed unmodified: like netsetup's own bridge naming, the derived VLAN
# sub-interface name ("$VNAP_UPLINK_IFACE.<vlan_id>") must fit Linux's
# IFNAMSIZ-1 (15 chars) limit -- a long uplink interface name combined
# with a 4-digit vlan_id can exceed it, which surfaces as an ordinary `ip
# link add` failure below, not something this script pre-validates.
set -eu

verb="$1"
req="$(cat)"

json() { printf '%s' "$req" | jq -r ".$1 // empty"; }

: "${VNAP_UPLINK_IFACE:?vlan-trunk.sh: VNAP_UPLINK_IFACE must be set to this host's VLAN-trunked uplink interface name}"

tap="$(json tap_name)"
# The VLAN ID is whatever the Subnet's NetworkClass allocated under the
# name "vlan_id" (an integer pool, or a static entries pool tuple -- see
# docs/specs/network.md); kyuusha itself attaches no meaning to it.
vlan_id="$(json subnet_values.vlan_id)"
bridge="kbr${vlan_id}"
sub="${VNAP_UPLINK_IFACE}.${vlan_id}"

case "$verb" in
attach)
	if [ -z "$vlan_id" ]; then
		echo "vlan-trunk: attach requires subnet_values.vlan_id (give the NetworkClass a pool named vlan_id)" >&2
		exit 1
	fi
	out="$(ip link add "$bridge" type bridge 2>&1)" || case "$out" in
	*"File exists"*) ;;
	*) echo "vlan-trunk: $out" >&2; exit 1 ;;
	esac
	if ! ip link set "$bridge" up; then
		echo "vlan-trunk: ip link set $bridge up failed" >&2
		exit 1
	fi

	if ! ip link set "$VNAP_UPLINK_IFACE" up; then
		echo "vlan-trunk: ip link set $VNAP_UPLINK_IFACE up failed" >&2
		exit 1
	fi
	out="$(ip link add link "$VNAP_UPLINK_IFACE" name "$sub" type vlan id "$vlan_id" 2>&1)" || case "$out" in
	*"File exists"*) ;;
	*) echo "vlan-trunk: $out" >&2; exit 1 ;;
	esac
	if ! ip link set "$sub" master "$bridge"; then
		echo "vlan-trunk: attach $sub to $bridge failed" >&2
		exit 1
	fi
	if ! ip link set "$sub" up; then
		echo "vlan-trunk: ip link set $sub up failed" >&2
		exit 1
	fi

	if ! ip link set "$tap" master "$bridge"; then
		echo "vlan-trunk: attach $tap to $bridge failed" >&2
		exit 1
	fi
	if ! ip link set "$tap" up; then
		echo "vlan-trunk: ip link set $tap up failed" >&2
		exit 1
	fi
	;;

detach)
	# Nothing to do: the tap's own bridge membership ends when
	# netsetup.DeleteTap's "ip link delete $tap" removes it right after
	# this, same as the built-in Linux-bridge path's own detach (see
	# netsetup.DeleteTap's doc comment). The shared bridge and its VLAN
	# sub-interface are left in place for the next VM on this vlan_id,
	# same "never torn down, harmless to leave around" choice
	# netsetup's own ensureBridge makes.
	;;

*)
	echo "vlan-trunk: unknown verb '$verb' (want attach|detach)" >&2
	exit 1
	;;
esac
