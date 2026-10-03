#!/bin/sh
# frr-vrf-host-route.sh -- a reference VNAP (VM Network Attach Protocol; see
# docs/architecture.md「VMのネットワーク接続をCNIのようにプラガブルにすべきか」
# and docs/specs/vnap.md) plugin for a pure-L3, per-tenant-VRF deployment
# where each VM's /32 is injected directly into FRR's RIB (see
# docs/network-deployment-guide.md「3.5. Pure L3デプロイの場合」).
#
# This script's job stops at the VRF boundary: it only ever talks to FRR's
# RIB (a static route in one VRF) -- how that route actually reaches other
# hosts is entirely the network team's choice, and this same script works
# unchanged under either one:
#   - playground/evpn-vxlan-clos/: BGP EVPN Type-5 + a real VXLAN VTEP
#   - playground/vrf-lite-clos/: plain VRF-scoped `address-family ipv4
#     unicast` eBGP relay, no EVPN/VXLAN at all
# (see each lab's own README/run-test.sh for the respective leaf/spine
# config). If your Subnet's tenants never need overlapping address space at
# all, you don't need a VRF in the first place -- see
# examples/vnap-plugins/frr-ipv4-unicast.sh instead.
#
# Unlike the built-in Linux-bridge implementation (one shared bridge per
# Subnet, holding that Subnet's gateway_ip -- a real local L2 domain every
# VM on that Subnet, on this host, sits on), this gives each VM's tap its
# own point-to-point-shaped presence: no bridge, no shared L2 domain at
# all, matching this model's "no L2 stretch needed, route by exact /32 host
# route" shape. Per VM, "attach":
#
#   1. assigns the Subnet's gateway_ip as a /32 address directly on the
#      tap, so the kernel natively answers the guest's ARP for its own
#      gateway -- the guest's own network-config is an ordinary subnet
#      CIDR + gateway4, completely unchanged from the built-in Linux-bridge
#      path (see internal/compute-agent/vmm/seed.go's buildNetworkConfig)
#   2. enables proxy_arp on the tap, so the guest's ARP for another VM it
#      believes is on-link (same Subnet CIDR, per its own network-config)
#      gets answered with this tap's own MAC too -- the kernel then
#      IP-forwards the frame via that destination's own /32 route (added
#      by this same script, for that VM's own attach -- possibly via a
#      different tap on this same host, or learned from FRR/BGP if it's on
#      a different Hypervisor) rather than ever needing a real shared L2
#      segment
#   3. enslaves the tap into the tenant's VRF device (`ip link set $tap
#      master $vrf`) -- REQUIRED for the next step: a Linux kernel VRF
#      routing table only ever resolves a route whose output device is
#      itself a member of that VRF. Skip this and FRR will accept the
#      "ip route ... vrf ..." config below with no error, but the route
#      never actually installs into the kernel/RIB -- a silent no-op that
#      was this script's own bug until verified against a real BGP EVPN
#      lab (see docs/release-notes.md), not something to reintroduce.
#      Requires the vrf device to already exist on this host, matching
#      the FRR-side `vrf vrfNNNNNNNNNNNN` stanza below -- this script
#      never creates it (a per-tenant VRF is provisioned once by the
#      network team, shared by every VM of that tenant, not per-VM)
#   4. injects the VM's own IP (/32, via this tap) into FRR (via vtysh,
#      into the same per-tenant VRF) as a static route. This is the ONLY
#      place that route is installed: do not also add it directly into
#      the kernel's own VRF routing table (e.g. a bare `ip route replace
#      ${ip}/32 dev $tap`) alongside this -- zebra treats a kernel-sourced
#      route for the same prefix as a lower-distance "K" route that wins
#      over FRR's own "S" static route in the RIB, so the static route
#      stops being the selected/best path and `redistribute static` never
#      fires for it. FRR installs its own selected static route into the
#      kernel FIB itself once vtysh below succeeds, so there is nothing
#      left for this script to add by hand
#
# This script only ever talks to FRR's RIB (a static route in one VRF) --
# it is NOT responsible for BGP/EVPN configuration itself (VRF/RD/RT
# definitions, or the "redistribute static" policy that actually turns
# these static routes into actual route advertisements). That remains the
# network team's job, same as the rest of docs/network-deployment-guide.md
# -- see the REQUIRED companion FRR config sketch at the bottom of this
# file.
#
# Requires (on whatever host runs compute-agent with
# -network-attach-bin=/path/to/this/script): a real iproute2 `ip` (not
# busybox's -- needs `ip addr replace`/`ip route replace`), `sysctl`, `jq`,
# and FRR's `vtysh`, all reachable, running as a user with CAP_NET_ADMIN
# (root is simplest).
#
# This is a REFERENCE implementation, meant to be read and adapted, not
# deployed unmodified: it does no locking against concurrent attach/detach
# for different VMs (vtysh serializes its own config changes, but this
# script's own kernel-side steps are not transactional across the three
# ip/sysctl calls), and the "vrfNNNNNNNNNNNN" VRF-naming convention below
# is this script's own invention -- match it to whatever your own FRR VRF
# naming actually is.
set -eu

verb="$1"
req="$(cat)"

json() { printf '%s' "$req" | jq -r ".$1 // empty"; }

tap="$(json tap_name)"
tenant_id="$(json tenant_id)"

# Mirrors internal/compute-agent/netsetup.TapName's own reasoning: a VRF
# name may need to double as a Linux VRF net-device name (IFNAMSIZ-1 = 15
# chars), too short for kyuusha's own tenant_id ("tenant-<16 hex chars>").
vrf="vrf$(printf '%s' "$tenant_id" | sha256sum | cut -c1-12)"

case "$verb" in
attach)
	ip_addr="$(json ip_address)"
	gateway_ip="$(json gateway_ip)"
	if [ -z "$ip_addr" ] || [ -z "$gateway_ip" ]; then
		echo "frr-vrf-host-route: attach requires ip_address and gateway_ip" >&2
		exit 1
	fi

	cleanup() {
		ip addr del "${gateway_ip}/32" dev "$tap" 2>/dev/null || true
	}

	if ! ip link set "$tap" up; then
		echo "frr-vrf-host-route: ip link set $tap up failed" >&2
		exit 1
	fi
	# Enslave into the tenant VRF before any addr/route step below, so
	# those operate on an already-vrf-scoped device rather than relying on
	# the kernel's route-migration behavior when a device changes VRF
	# membership later. See this package's own doc comment for why this
	# step, once missing, made every route below a silent no-op.
	if ! ip link set "$tap" master "$vrf"; then
		echo "frr-vrf-host-route: enslave $tap into vrf $vrf failed (does the vrf device exist yet?)" >&2
		exit 1
	fi
	# "replace", not "add": attach must be idempotent (see this script's
	# doc comment and docs/specs/vnap.md「冪等性」) -- a resent
	# CreateCommand re-invokes Wire for a tap already wired.
	if ! ip addr replace "${gateway_ip}/32" dev "$tap"; then
		echo "frr-vrf-host-route: assign gateway_ip to $tap failed" >&2
		exit 1
	fi
	if ! sysctl -qw "net.ipv4.ip_forward=1"; then
		echo "frr-vrf-host-route: enable ip_forward failed" >&2
		cleanup
		exit 1
	fi
	if ! sysctl -qw "net.ipv4.conf.${tap}.proxy_arp=1"; then
		echo "frr-vrf-host-route: enable proxy_arp on $tap failed" >&2
		cleanup
		exit 1
	fi
	if ! vtysh -c "configure terminal" -c "vrf ${vrf}" \
		-c "ip route ${ip_addr}/32 ${tap}" -c "end"; then
		echo "frr-vrf-host-route: FRR route injection for $ip_addr failed" >&2
		cleanup
		exit 1
	fi
	;;

detach)
	# detach's payload never carries ip_address (see docs/specs/vnap.md:
	# removing a port never needs to know what it used to be configured
	# with) -- recover it from the kernel's own /32 route
	# for this tap instead, which (unlike a bridge attachment) this script
	# itself installed at attach time and which still exists at this point
	# (netsetup.DeleteTap calls detach before removing the tap device).
	# iproute2 prints a /32 host route without the "/32" suffix (e.g. "10.9.9.5
	# scope link"), so this takes the first field as-is rather than grepping
	# for one.
	ip_addr="$(ip -4 -o route show dev "$tap" 2>/dev/null | awk '{print $1}' | head -n1)"
	if [ -n "$ip_addr" ]; then
		vtysh -c "configure terminal" -c "vrf ${vrf}" \
			-c "no ip route ${ip_addr}/32 ${tap}" -c "end" 2>/dev/null || true
	fi
	# The tap device itself (and everything tied to its lifetime -- its
	# kernel /32 route, its own proxy_arp sysctl entry) is removed right
	# after this by netsetup.DeleteTap's own "ip link delete"; only FRR's
	# separately-held RIB entry needed this script's help to clean up.
	;;

*)
	echo "frr-vrf-host-route: unknown verb '$verb' (want attach|detach)" >&2
	exit 1
	;;
esac

# --- Required companion FRR config (sketch, not exhaustive) ---
#
# This sketch is the EVPN Type-5 + VXLAN realization specifically (verified
# end-to-end against playground/evpn-vxlan-clos/, a real containerlab BGP
# EVPN lab -- leaf-spine-leaf, unnumbered eBGP, one ASN per hypervisor; see
# docs/release-notes.md). A few pieces below are easy to miss and silently
# leave routes unadvertised rather than erroring, so they're spelled out
# here even though this is "the network team's job, not this script's".
#
# playground/vrf-lite-clos/ verifies a much shorter alternative: no EVPN,
# no VXLAN, no SVI/VNI at all -- every hop (not just the two edge hosts)
# carries this same VRF and relays the route via a plain
# `address-family ipv4 unicast` eBGP session scoped to that VRF. This
# script's own attach/detach logic above is identical either way.
#
# ip link add vrfNNNNNNNNNNNN type vrf table <per-tenant table id>
# ip link set vrfNNNNNNNNNNNN up
# !
# ip link add br-vrfNNNNNNNNNNNN type bridge          ! the "SVI" -- an
# ip link set br-vrfNNNNNNNNNNNN master vrfNNNNNNNNNNNN ! L3VNI needs one to
# ip link set br-vrfNNNNNNNNNNNN up                    ! reach evpn "State:
#                                                       ! Up" even with no
#                                                       ! real L2 traffic on
#                                                       ! it (FRR's
#                                                       ! symmetric-IRB
#                                                       ! model). Must be a
#                                                       ! VRF member itself
#                                                       ! -- otherwise a
#                                                       ! decapsulated
#                                                       ! packet's route
#                                                       ! lookup lands in
#                                                       ! the wrong (default)
#                                                       ! table and is
#                                                       ! silently dropped
# ip link add vxlanNNNNNN type vxlan id <L3VNI> dstport 4789 local <this
#   host's own VTEP/loopback IP> nolearning
# ip link set vxlanNNNNNN master br-vrfNNNNNNNNNNNN
# ip link set vxlanNNNNNN up
# !
# vrf vrfNNNNNNNNNNNN
#  vni <per-tenant L3VNI, network team's own numbering>
# exit-vrf
# !
# router bgp <local ASN>
#  neighbor <iface> interface remote-as external   ! per-hop underlay eBGP
#  address-family ipv4 unicast
#   redistribute connected   ! so every hop can route to every VTEP's own
#  exit-address-family       ! loopback -- without this, nothing has a path
#                             ! to the VXLAN destination IP at all
#  address-family l2vpn evpn
#   neighbor <iface> activate   ! on every eBGP session, including transit
#   advertise-all-vni           ! hops -- needed for VNI/VRF recognition,
#  exit-address-family          ! not just route origination
# !
# router bgp <local ASN> vrf vrfNNNNNNNNNNNN
#  address-family ipv4 unicast
#   redistribute static
#  exit-address-family
#  address-family l2vpn evpn
#   route-target both <RT>   ! must match across every host/leaf/spine that
#   advertise ipv4 unicast   ! shares this tenant -- FRR's own ASN:VNI
#  exit-address-family       ! auto-derived RT differs per host otherwise
# !
#
# See docs/network-deployment-guide.md for the surrounding RT/RD
# requirements this config sits inside of (per-tenant RT unique across AZs,
# RD that does NOT derive from kyuusha's vlan_id alone).
