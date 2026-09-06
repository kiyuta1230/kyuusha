#!/bin/sh
# Runs as PID 1 (boot_args: init=/init) inside the playground's Firecracker
# guest -- see docs/specs/firecracker-boot.md. Deliberately not a real init
# system (no service management, doesn't reap zombies): its jobs are to
# prove the guest kernel actually booted (a recognizable line on the serial
# console, ttyS0, which compute-agent captures to <fc-run-dir>/<vm_id>/
# console.log), and -- for every real network interface fcvmm wired for
# this VM (see internal/compute-agent/netsetup and docs/specs/network.md)
# -- configure it with the static IP compute-agent baked into the kernel
# cmdline and prove reachability by pinging its gateway.
#
# There's no DHCP server anywhere in this system and no guarantee the guest
# kernel has IP autoconfiguration (CONFIG_IP_PNP) built in, so IP
# configuration travels as compute-agent's own kyuusha.net.<index>.*
# cmdline convention (not a real Linux kernel parameter) instead, parsed
# below directly from /proc/cmdline.
mount -t proc proc /proc
mount -t sysfs sysfs /sys

echo "kyuusha: firecracker guest booted OK, uptime=$(cut -d' ' -f1 /proc/uptime)s"

i=0
while [ $i -lt 8 ]; do
  ip_val=""
  gw_val=""
  primary_val=""
  for arg in $(cat /proc/cmdline); do
    case "$arg" in
      kyuusha.net.$i.ip=*) ip_val=${arg#*=} ;;
      kyuusha.net.$i.gw=*) gw_val=${arg#*=} ;;
      kyuusha.net.$i.primary=*) primary_val=${arg#*=} ;;
    esac
  done

  if [ -n "$ip_val" ]; then
    ip link set "eth$i" up
    ip addr add "$ip_val" dev "eth$i"
    echo "kyuusha: eth$i configured ip=$ip_val"

    if [ -n "$gw_val" ]; then
      if [ "$primary_val" = "1" ]; then
        ip route add default via "$gw_val"
      fi
      if ping -c 1 -W 2 "$gw_val" >/dev/null 2>&1; then
        echo "kyuusha: eth$i reached gateway $gw_val OK"
      else
        echo "kyuusha: eth$i FAILED to reach gateway $gw_val"
      fi
    fi
  fi

  i=$((i + 1))
done

while true; do
  sleep 3600
done
