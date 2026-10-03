#!/bin/sh
# frr-ipv4-unicast.sh -- a reference VNAP (VM Network Attach Protocol; see
# docs/architecture.md「VMのネットワーク接続をCNIのようにプラガブルにすべきか」
# and docs/specs/vnap.md) plugin for a pure-L3 deployment where every
# tenant's address space is guaranteed non-overlapping across the whole
# fabric -- so, unlike frr-vrf-host-route.sh, there is no per-tenant VRF
# anywhere: every VM's /32 is injected straight into FRR's one shared
# (default) routing table and relayed by plain
# `address-family ipv4 unicast` BGP, hop by hop, with no encapsulation at
# all (see docs/network-deployment-guide.md「3.5. Pure L3デプロイの場合」and
# playground/ipv4-unicast-clos/, which verifies this end-to-end against a
# real leaf-spine-leaf fabric).
#
# If your tenants' Subnets can ever reuse the same private address range
# (the normal case for kyuusha, where each tenant picks its own CIDR), this
# script is the wrong choice -- use frr-vrf-host-route.sh instead, which
# keeps every tenant inside its own VRF so overlapping addresses never
# collide. This script's entire simplicity comes from *not* needing that:
# tenant isolation here is delegated elsewhere (unique-address allocation
# in kyuusha's own IPAM, and the tap-side default-deny ACL baseline SNAP/
# nftacl already enforces regardless of what the network does -- see
# docs/architecture.md「防御層としてのNetworkInterface ACL」). Losing either
# of those two backstops turns a misconfiguration into a real cross-tenant
# leak, since there is no VRF here to catch it.
#
# Unlike frr-vrf-host-route.sh, this makes the hypervisor a genuine L3
# gateway for the whole Subnet, not just a /32 point-to-point peer. Per VM,
# "attach":
#
#   1. assigns gateway_ip to the tap with the Subnet's *real* prefix_len
#      (e.g. a /24, not a /32) -- the tap now holds a real connected route
#      for the whole Subnet CIDR, same as a textbook router interface. The
#      guest's own network-config is unchanged (ordinary subnet CIDR +
#      gateway4, see internal/compute-agent/vmm/seed.go's
#      buildNetworkConfig)
#   2. enables proxy_arp on the tap, same reasoning as
#      frr-vrf-host-route.sh: the guest's ARP for another VM it believes is
#      on-link gets answered by this tap, and the kernel IP-forwards the
#      frame via that VM's own /32 (installed on whichever host actually
#      holds it, learned via BGP if remote). Caveat this script's gateway
#      choice adds that the /32-only design didn't have: because the tap
#      now owns a connected route for the *entire* Subnet CIDR, proxy_arp
#      will also answer for addresses in that CIDR that don't correspond
#      to any real VM anywhere (frr-vrf-host-route.sh's /32-gateway avoids
#      this since it has no broader connected route to match against) --
#      a real but narrow trade-off for actually being a correct gateway
#   3. injects the VM's own IP (/32, via this tap) into FRR as a plain
#      static route -- no VRF, no vtysh `vrf` context, just the default
#      routing table -- which `redistribute static` then turns into a real
#      BGP advertisement. This is the ONLY place that route is installed:
#      do not also add it directly into the kernel's own routing table
#      (e.g. a bare `ip route replace ${ip}/32 dev $tap`) alongside this --
#      zebra treats a kernel-sourced route for the same prefix as a
#      lower-distance "K" route that wins over FRR's own "S" static route
#      in the RIB, so the static route stops being the selected/best path
#      and `redistribute static` never fires for it (found the hard way
#      against playground/ipv4-unicast-clos/: the route sat configured
#      but silently never reached the far host). FRR installs its own
#      selected static route into the kernel FIB itself once vtysh below
#      succeeds, so there is nothing left for this script to add by hand.
#
# This script is NOT responsible for the underlay BGP session itself (ASN
# assignment, unnumbered vs numbered, etc.) -- same boundary as
# frr-vrf-host-route.sh. One piece specific to this no-VRF design is worth
# calling out explicitly: since the hypervisor has no VRF of its own to
# fall back on for anything outside its own Subnet, it needs the network
# team's Leaf to hand it a real default route (`neighbor <host-facing
# iface> default-originate` on the Leaf side) so traffic leaving the local
# Subnet -- the shared NAT gateway, DNS resolver, another tenant's shared
# Subnet -- has somewhere to go. See the REQUIRED companion FRR config
# sketch at the bottom of this file, and playground/ipv4-unicast-clos/
# which verifies the hypervisor actually receives and uses it.
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
# script's own kernel-side steps are not transactional across the several
# ip/sysctl calls).
set -eu

verb="$1"
req="$(cat)"

json() { printf '%s' "$req" | jq -r ".$1 // empty"; }

tap="$(json tap_name)"

case "$verb" in
attach)
	ip_addr="$(json ip_address)"
	gateway_ip="$(json gateway_ip)"
	prefix_len="$(json prefix_len)"
	if [ -z "$ip_addr" ] || [ -z "$gateway_ip" ] || [ -z "$prefix_len" ]; then
		echo "frr-ipv4-unicast: attach requires ip_address, gateway_ip, and prefix_len" >&2
		exit 1
	fi

	cleanup() {
		ip addr del "${gateway_ip}/${prefix_len}" dev "$tap" 2>/dev/null || true
	}

	if ! ip link set "$tap" up; then
		echo "frr-ipv4-unicast: ip link set $tap up failed" >&2
		exit 1
	fi
	# "replace", not "add": attach must be idempotent (see this script's
	# doc comment and docs/specs/vnap.md「冪等性」) -- a resent
	# CreateCommand re-invokes Wire for a tap already wired. Real
	# prefix_len (not /32) -- see this file's own header comment for why.
	if ! ip addr replace "${gateway_ip}/${prefix_len}" dev "$tap"; then
		echo "frr-ipv4-unicast: assign gateway_ip to $tap failed" >&2
		exit 1
	fi
	if ! sysctl -qw "net.ipv4.ip_forward=1"; then
		echo "frr-ipv4-unicast: enable ip_forward failed" >&2
		cleanup
		exit 1
	fi
	if ! sysctl -qw "net.ipv4.conf.${tap}.proxy_arp=1"; then
		echo "frr-ipv4-unicast: enable proxy_arp on $tap failed" >&2
		cleanup
		exit 1
	fi
	# No vrf context -- this goes straight into FRR's default instance
	# (`router bgp <ASN>`'s own static routes, redistributed under its
	# plain `address-family ipv4 unicast`).
	if ! vtysh -c "configure terminal" -c "ip route ${ip_addr}/32 ${tap}" -c "end"; then
		echo "frr-ipv4-unicast: FRR route injection for $ip_addr failed" >&2
		cleanup
		exit 1
	fi
	;;

detach)
	# Mirrors frr-vrf-host-route.sh's own detach: recover ip_addr from the
	# kernel's own /32 route (detach's payload never carries it, see
	# docs/specs/vnap.md) rather than needing it passed in.
	ip_addr="$(ip -4 -o route show dev "$tap" 2>/dev/null | awk '{print $1}' | head -n1)"
	if [ -n "$ip_addr" ]; then
		vtysh -c "configure terminal" -c "no ip route ${ip_addr}/32 ${tap}" -c "end" 2>/dev/null || true
	fi
	# The tap device itself (and everything tied to its lifetime -- its
	# kernel /32 route, its gateway_ip/prefix_len address, its own
	# proxy_arp sysctl entry) is removed right after this by
	# netsetup.DeleteTap's own "ip link delete"; only FRR's separately-held
	# RIB entry needed this script's help to clean up.
	;;

*)
	echo "frr-ipv4-unicast: unknown verb '$verb' (want attach|detach)" >&2
	exit 1
	;;
esac

# --- Required companion FRR config (sketch, not exhaustive) ---
#
# Verified end-to-end against playground/ipv4-unicast-clos/ (containerlab,
# leaf-spine-leaf, unnumbered eBGP, one ASN per hypervisor -- matching
# docs/network-deployment-guide.md「3.5. Pure L3デプロイの場合」's recommended reference
# config). No VRF, no EVPN, no VXLAN anywhere in this one -- every hop just
# relays plain unicast IPv4 routes, and the actual data packets cross the
# fabric completely unencapsulated.
#
# interface <host-facing iface>
# !
# router bgp <Leaf's ASN>
#  neighbor <host-facing iface> interface remote-as external
#  !
#  address-family ipv4 unicast
#   neighbor <host-facing iface> activate
#   neighbor <host-facing iface> default-originate   ! REQUIRED: the
#  exit-address-family                               ! hypervisor has no
#                                                     ! VRF of its own to
#                                                     ! fall back on, so
#                                                     ! without this it has
#                                                     ! no route at all to
#                                                     ! anything outside
#                                                     ! its local Subnet
#                                                     ! (shared NAT
#                                                     ! gateway, DNS
#                                                     ! resolver, etc.)
# !
#
# On the hypervisor side itself (this script's own host, set up once by
# the network team -- not per-VM, not this script's job):
#
# router bgp <this hypervisor's own ASN>
#  neighbor <leaf-facing iface> interface remote-as external
#  !
#  address-family ipv4 unicast
#   redistribute static   ! turns this script's `ip route ... dev <tap>`
#   neighbor <leaf-facing iface> activate   ! calls above into real
#  exit-address-family                      ! advertisements
# !
#
# See docs/network-deployment-guide.md for the ASN-numbering guidance this
# config sits inside of (one ASN per hypervisor, sized for your fleet).
