#!/usr/bin/env bash
# playground/churn.sh -- continuously creates and deletes VMs (each with an
# attached Volume) against the running playground stack, so the fleet/
# virtual-machines Grafana dashboards (docs/specs/observability-metrics.md)
# show constantly-moving numbers instead of a static snapshot. Meant to run
# alongside playground/demo.sh, or on its own while watching the
# dashboards -- NOT a correctness test (see playground/scenario.sh) and NOT
# a load test (the interval is paced for a human watching a dashboard, not
# for maximizing throughput).
#
# Usage: playground/churn.sh [max_vms] [interval_seconds]
# Ctrl+C (or any TERM) cleans up everything this script created -- deletes
# the whole tenant it made, which takes every VM/Volume/VolumeAttachment/
# Image under it with it -- before exiting, so repeated runs never pile up
# leftover drift the way a lot of this session's own manual testing did.
set -uo pipefail # not -e: one failed CLI call (e.g. a transient race)
                 # should log and move on to the next iteration, not kill
                 # the whole churn loop
cd "$(dirname "$0")/.."

max_vms="${1:-3}"
interval="${2:-4}"

echo "==> churn.sh: keeping up to $max_vms VM(+Volume) slots churning, ${interval}s between actions"
echo "    (Ctrl+C to stop and clean up everything this script created)"

if ! (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; then
  echo "!! api-gateway is not reachable at localhost:8080 -- start the stack first (playground/demo.sh or docker compose up)" >&2
  exit 1
fi
exec 3>&-

# churn.sh is meant to run indefinitely (that's the whole point -- keep the
# fleet dashboards moving), so tokens are minted with a long TTL and are
# additionally re-minted periodically (see refresh_tokens_if_stale below).
# Without this, a run longer than the CLI's default 1h -ttl silently turns
# into a no-op loop that only logs "!! ... token is expired" -- and worse,
# cleanup()'s own delete calls use the same tokens, so Ctrl+C after that
# point can't clean up what it created either, leaving an orphaned tenant
# behind. Both bit us for real on 2026-09-13.
token_ttl="24h"
token_refresh_after=$((12 * 3600)) # re-mint well before token_ttl elapses
token_minted_at=0

admin_token="$(go run ./cmd/kyuusha token mint -tenant=bootstrap-admin -role=admin -sub=churn-admin@example.com -ttl="$token_ttl")"
tenant_name="churn-$(date +%s)"
tenant_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant create -addr=localhost:8080 \
  -name="$tenant_name" -display-name="Churn Demo" \
  -max-vcpu=$((max_vms * 2)) -max-memory-mb=$((max_vms * 512)) -max-volume-gb=$((max_vms * 2)) \
  -max-vms="$max_vms" -max-vcpu-per-vm=1 -max-memory-mb-per-vm=256)"
tenant="$(echo "$tenant_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
echo "==> tenant=$tenant"
export KYUUSHA_TOKEN
KYUUSHA_TOKEN="$(go run ./cmd/kyuusha token mint -tenant="$tenant" -ttl="$token_ttl")"
token_minted_at=$(date +%s)

refresh_tokens() {
  admin_token="$(go run ./cmd/kyuusha token mint -tenant=bootstrap-admin -role=admin -sub=churn-admin@example.com -ttl="$token_ttl")"
  KYUUSHA_TOKEN="$(go run ./cmd/kyuusha token mint -tenant="$tenant" -ttl="$token_ttl")"
  token_minted_at=$(date +%s)
  echo "==> tokens refreshed (next refresh in ~$((token_refresh_after / 3600))h)"
}

refresh_tokens_if_stale() {
  if (($(date +%s) - token_minted_at >= token_refresh_after)); then
    refresh_tokens
  fi
}

image_line="$(go run ./cmd/kyuusha image create -addr=localhost:8080 -tenant="$tenant" -name=churn-image \
  -format=kernel_rootfs -kernel-url=http://image-assets/vmlinux -rootfs-url=http://image-assets/rootfs.ext4)"
image="$(echo "$image_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
phase=""
for _ in $(seq 1 30); do
  phase="$(go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant" -id="$image" | grep -o 'phase=[^ ]*' | cut -d= -f2)"
  [ "$phase" = "Ready" ] && break
  sleep 1
done
echo "==> image=$image phase=$phase"

if ! KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha storageconn list -addr=localhost:8080 2>/dev/null | grep -q 'name=playground-nfs'; then
  KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha storageconn create -addr=localhost:8080 -name=playground-nfs -zones=zone-a >/dev/null
fi
mkdir -p playground/volume-data
chmod 0777 playground/volume-data

# vm_ids[i]/volume_ids[i]/attach_ids[i] track one "slot" each -- empty
# ("") until create_slot fills it, emptied again by delete_slot.
declare -a vm_ids volume_ids attach_ids
for ((i = 0; i < max_vms; i++)); do
  vm_ids[i]=""
  volume_ids[i]=""
  attach_ids[i]=""
done

n=0
create_slot() {
  local i="$1" suffix vm_line vm_id identifier vol_line vol_id vol_phase attach_line attach_id
  suffix="${tenant_name}-$((n++))"
  echo "==> [$i] creating VM+Volume+VolumeAttachment ($suffix)"

  vm_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name="vm-$suffix" \
    -image="$image" -vcpu=1 -memory-mb=128 -wait 2>&1)" || {
    echo "!! vm create failed: $vm_line" >&2
    return
  }
  vm_id="$(echo "$vm_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
  if [ -z "$vm_id" ]; then
    echo "!! could not parse vm_id from: $vm_line" >&2
    return
  fi

  identifier="churn-$suffix.img"
  truncate -s 1M "playground/volume-data/$identifier"
  chmod 0666 "playground/volume-data/$identifier"
  vol_line="$(go run ./cmd/kyuusha volume create -addr=localhost:8080 -tenant="$tenant" -name="vol-$suffix" \
    -size-gb=1 -protocol=NFS -storage-connection=playground-nfs -identifier="$identifier" 2>&1)" || {
    echo "!! volume create failed: $vol_line" >&2
  }
  vol_id="$(echo "$vol_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
  for _ in $(seq 1 20); do
    vol_phase="$(go run ./cmd/kyuusha volume get -addr=localhost:8080 -tenant="$tenant" -id="$vol_id" 2>/dev/null | grep -o 'phase=[^ ]*' | cut -d= -f2)"
    [ "$vol_phase" = "Ready" ] && break
    sleep 1
  done

  attach_line="$(go run ./cmd/kyuusha volattach create -addr=localhost:8080 -tenant="$tenant" -name="attach-$suffix" \
    -vm="$vm_id" -volume="$vol_id" 2>&1)" || echo "!! volattach create failed: $attach_line" >&2
  attach_id="$(echo "$attach_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"

  vm_ids[i]="$vm_id"
  volume_ids[i]="$vol_id"
  attach_ids[i]="$attach_id"
  echo "    [$i] vm=$vm_id volume=$vol_id attach=$attach_id"
}

delete_slot() {
  local i="$1"
  echo "==> [$i] deleting vm=${vm_ids[i]} volume=${volume_ids[i]} attach=${attach_ids[i]}"
  [ -n "${attach_ids[i]}" ] && go run ./cmd/kyuusha volattach delete -addr=localhost:8080 -tenant="$tenant" -id="${attach_ids[i]}" >/dev/null 2>&1
  [ -n "${volume_ids[i]}" ] && go run ./cmd/kyuusha volume delete -addr=localhost:8080 -tenant="$tenant" -id="${volume_ids[i]}" >/dev/null 2>&1
  [ -n "${vm_ids[i]}" ] && go run ./cmd/kyuusha vm delete -addr=localhost:8080 -tenant="$tenant" -id="${vm_ids[i]}" >/dev/null 2>&1
  vm_ids[i]=""
  volume_ids[i]=""
  attach_ids[i]=""
}

cleanup() {
  echo
  echo "==> churn.sh: stopping, cleaning up..."
  refresh_tokens # unconditional: guarantees deletes below work no matter how stale the old tokens are
  for ((i = 0; i < max_vms; i++)); do
    [ -n "${vm_ids[i]:-}" ] && delete_slot "$i"
  done
  go run ./cmd/kyuusha image delete -addr=localhost:8080 -tenant="$tenant" -id="$image" >/dev/null 2>&1
  KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant delete -addr=localhost:8080 -id="$tenant" >/dev/null 2>&1
  echo "==> done (tenant $tenant and everything under it removed)"
  exit 0
}
trap cleanup INT TERM

# Randomly pick a slot each tick: filling an empty one churns "up",
# emptying a full one churns "down" -- over time this oscillates the VM
# count around roughly half of max_vms rather than monotonically ramping
# up once and flatlining, which is more interesting to watch on a
# dashboard.
while true; do
  refresh_tokens_if_stale
  i=$((RANDOM % max_vms))
  if [ -z "${vm_ids[i]}" ]; then
    create_slot "$i"
  else
    delete_slot "$i"
  fi
  sleep "$interval"
done
