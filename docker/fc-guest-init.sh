#!/bin/sh
# Runs as PID 1 (boot_args: init=/init) inside the playground's guest --
# see docs/specs/firecracker-boot.md and docs/specs/qemu-boot.md. The same
# rootfs image boots under either driver_hint (FIRECRACKER via fcvmm, QEMU
# via qemuvmm); this script has no way to tell which one launched it, so it
# doesn't claim to know. Deliberately not a real init system (no service
# management, doesn't reap zombies): its jobs are to prove the guest kernel
# actually booted (a recognizable line on the serial console, ttyS0, which
# compute-agent captures to <run-dir>/<vm_id>/console.log), and -- for
# every real network interface the VMM driver wired for this VM (see
# internal/compute-agent/netsetup and docs/specs/network.md) -- configure
# it with the static IP compute-agent baked into the kernel cmdline and
# prove reachability by pinging its gateway.
#
# There's no DHCP server anywhere in this system and no guarantee the guest
# kernel has IP autoconfiguration (CONFIG_IP_PNP) built in, so IP
# configuration travels as compute-agent's own kyuusha.net.<index>.*
# cmdline convention (not a real Linux kernel parameter) instead, parsed
# below directly from /proc/cmdline.
mount -t proc proc /proc
mount -t sysfs sysfs /sys

echo "kyuusha: guest booted OK, uptime=$(cut -d' ' -f1 /proc/uptime)s"

# If compute-agent built a cloud-init NoCloud seed disk (spec.user_data was
# set -- see internal/compute-agent/fcvmm/seed.go and docs/architecture.md
# "UserData注入: NoCloud seed disk"), it's the second virtio-block device,
# /dev/vdb, formatted ext4 (not vfat/ISO9660 -- this kernel has neither
# CONFIG_VFAT_FS nor CONFIG_ISO9660_FS, only ext4; see
# internal/compute-agent/vmm/seed.go). This guest has no real cloud-init
# installed (it's a bare from-scratch Alpine rootfs, not a full distro
# image), so this isn't actually running user-data -- it's a
# minimal, honest stand-in that just proves the seed disk arrived and is
# readable, the same way the tap-wiring gateway ping proves that pipe
# end-to-end without needing a second VM.
if [ -b /dev/vdb ]; then
  mkdir -p /mnt/seed
  if mount -t ext4 -o ro /dev/vdb /mnt/seed 2>/dev/null; then
    echo "kyuusha: seed disk mounted (not real cloud-init -- proving delivery only)"
    if [ -f /mnt/seed/user-data ]; then
      while IFS= read -r line; do
        echo "kyuusha: user-data: $line"
      done < /mnt/seed/user-data
    fi
    if [ -f /mnt/seed/network-config ]; then
      echo "kyuusha: seed disk also carries network-config"
    fi
  else
    echo "kyuusha: seed disk present at /dev/vdb but could not be mounted"
  fi
fi

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
