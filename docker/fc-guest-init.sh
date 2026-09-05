#!/bin/sh
# Runs as PID 1 (boot_args: init=/init) inside the playground's Firecracker
# guest -- see docs/specs/firecracker-boot.md. Deliberately not a real init
# system (no service management, doesn't reap zombies): the only job here is
# to prove the guest kernel actually booted, by writing a recognizable line
# to the serial console (ttyS0), which compute-agent captures to
# <fc-run-dir>/<vm_id>/console.log.
mount -t proc proc /proc
mount -t sysfs sysfs /sys

echo "kyuusha: firecracker guest booted OK, uptime=$(cut -d' ' -f1 /proc/uptime)s"

while true; do
  sleep 3600
done
