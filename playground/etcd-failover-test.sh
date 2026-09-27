#!/usr/bin/env bash
# playground/etcd-failover-test.sh -- turns the playground's normally
# single-member etcd into a real 3-member cluster (see
# docker-compose.etcd-cluster.yml), identifies the current Raft leader,
# kills its container outright (docker kill, not a graceful stop -- this is
# meant to simulate a real node loss, not a clean shutdown), and confirms
# kyuusha's own services (not etcd's own Raft correctness, which is
# upstream's concern and not re-tested here) actually keep working through
# a real leader failover: existing long-lived etcd Watches (which is what
# every reconciler's main loop and every watchPendingX goroutine is built
# on, see internal/resource.Store.Watch) keep delivering events, and new
# Create/Get calls through api-gateway keep succeeding, all without
# needing any kyuusha process to be restarted.
#
# NOT a correctness regression test in the playground/scenario.sh sense
# (this isn't part of CI -- see docs/architecture.md「未決事項」and
# docs/release-notes.md for why: it exercises a real multi-second Raft
# election under a real container kill, which is exactly the kind of
# timing-sensitive, infrastructure-heavy scenario that doesn't belong on a
# shared CI runner) and NOT a load test (see churn.sh for that shape of
# tool) -- this is a manual, on-demand infrastructure-failure drill, run
# when you actually want to know if a real etcd node loss would take
# kyuusha down with it.
#
# Usage: playground/etcd-failover-test.sh
# Brings the stack up (or reuses it if already running with the cluster
# overlay applied) the first time this is run; leaves it running
# afterwards, same convention as scenario.sh/demo.sh, so you can keep
# poking at it. Re-run any time; each run creates and cleans up its own
# throwaway tenant.
set -euo pipefail
cd "$(dirname "$0")/.."

project="kyuusha-playground"
etcd_endpoints="etcd:2379,etcd-2:2379,etcd-3:2379"

echo "==> starting docker compose stack with the 3-member etcd cluster overlay"
KYUUSHA_ETCD_ENDPOINTS="$etcd_endpoints" docker compose -p "$project" \
  -f playground/docker-compose.yml -f playground/docker-compose.etcd-cluster.yml \
  up -d --build

echo "==> waiting for api-gateway's gRPC port to accept connections"
for _ in $(seq 1 60); do
  if (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; then
    exec 3>&-
    break
  fi
  sleep 1
done

# current_leader_container prints the compose container name (etcd/etcd-2/
# etcd-3's own "-1" replica suffix included) of whichever of the 3 members
# currently reports itself as Raft leader. `-w simple` is a stable,
# trivially-parseable comma-separated format (unlike `-w table`'s
# box-drawing columns): endpoint,member_id,version,db_size,is_leader,...
#
# Queries via whichever member container is still running, tried in turn
# (`docker exec` on a dead container just fails, moving on to the next) --
# querying always through a hardcoded single container would itself break
# the moment that specific container is the one we killed.
current_leader_container() {
  local query_container endpoint
  for query_container in "${project}-etcd-1" "${project}-etcd-2-1" "${project}-etcd-3-1"; do
    endpoint="$(docker exec "$query_container" etcdctl --endpoints="http://$etcd_endpoints" \
      endpoint status --cluster -w simple 2>/dev/null | awk -F', ' '$5 == "true" { print $1 }')" || true
    [ -n "$endpoint" ] && break
  done
  endpoint="${endpoint#http://}"
  case "$endpoint" in
    etcd:*) echo "${project}-etcd-1" ;;
    etcd-2:*) echo "${project}-etcd-2-1" ;;
    etcd-3:*) echo "${project}-etcd-3-1" ;;
    *) return 1 ;;
  esac
}

echo "==> waiting for the 3-member cluster to report a stable leader"
leader=""
for _ in $(seq 1 30); do
  leader="$(current_leader_container || true)"
  [ -n "$leader" ] && break
  sleep 1
done
if [ -z "$leader" ]; then
  echo "!! cluster never reported a leader" >&2
  exit 1
fi
echo "    current leader: $leader"

admin_token="$(go run ./cmd/kyuusha token mint -tenant=bootstrap-admin -role=admin -sub=etcd-failover-admin@example.com)"
tenant_name="etcd-failover-$(date +%s)"
tenant_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant create -addr=localhost:8080 \
  -name="$tenant_name" -display-name="etcd failover test" \
  -max-vcpu=8 -max-memory-mb=8192 -max-vms=4 -max-vcpu-per-vm=2 -max-memory-mb-per-vm=2048 -max-images=2)"
echo "$tenant_line"
tenant="$(echo "$tenant_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"

cleanup() {
  echo "==> cleaning up tenant $tenant"
  KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant delete -addr=localhost:8080 -id="$tenant" >/dev/null 2>&1 || true
}
trap cleanup EXIT

export KYUUSHA_TOKEN
KYUUSHA_TOKEN="$(go run ./cmd/kyuusha token mint -tenant="$tenant")"

echo "==> creating Image and waiting for Ready (establishes real state before the failover)"
image_line="$(go run ./cmd/kyuusha image create -addr=localhost:8080 -tenant="$tenant" -name=etcd-failover-image \
  -format=kernel_rootfs -kernel-url=http://image-assets/vmlinux -rootfs-url=http://image-assets/rootfs.ext4)"
image="$(echo "$image_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
for _ in $(seq 1 30); do
  phase="$(go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant" -id="$image" | grep -o 'phase=[^ ]*' | cut -d= -f2)"
  [ "$phase" = "Ready" ] && break
  sleep 1
done
[ "$phase" = "Ready" ] || { echo "!! image never reached Ready (phase=$phase)" >&2; exit 1; }

echo "==> creating vm-before-failover and confirming it reaches Running (the real client -> Watch -> compute-reconciler -> compute-agent path, all working before we break anything)"
before_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-before-failover \
  -image="$image" -vcpu=1 -memory-mb=128 -wait | tail -1)"
echo "$before_line"
echo "$before_line" | grep -q 'phase=Running' || { echo "!! vm-before-failover did not reach Running: $before_line" >&2; exit 1; }

echo "==> starting a background 'kyuusha vm watch' to observe whether it keeps delivering events straight through the leader kill (internal/resource.Store.Watch has no reconnect-of-its-own logic -- this is testing whether the underlying etcd client's watch stream survives on its own, and whether callers like compute.Reconciler.Run notice if it doesn't)"
watch_log="$(mktemp)"
go run ./cmd/kyuusha vm watch -addr=localhost:8080 -tenant="$tenant" >"$watch_log" 2>&1 &
watch_pid=$!
sleep 2 # let the watch actually establish before we do anything

echo "==> killing the current leader ($leader) outright -- simulating real node loss, not a graceful shutdown"
docker kill "$leader" >/dev/null
killed_at=$(date +%s)

echo "==> waiting for the remaining 2 members to elect a new leader"
new_leader=""
for _ in $(seq 1 30); do
  new_leader="$(current_leader_container 2>/dev/null || true)"
  [ -n "$new_leader" ] && [ "$new_leader" != "$leader" ] && break
  new_leader=""
  sleep 1
done
if [ -z "$new_leader" ]; then
  echo "!! no new leader elected within 30s of killing $leader" >&2
  exit 1
fi
elected_after=$(($(date +%s) - killed_at))
echo "    new leader: $new_leader (elected ${elected_after}s after the kill)"

echo "==> confirming kyuusha's control plane still works with the old leader gone: creating vm-after-failover"
after_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-after-failover \
  -image="$image" -vcpu=1 -memory-mb=128 -wait | tail -1)"
echo "$after_line"
after_id="$(echo "$after_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if ! echo "$after_line" | grep -q 'phase=Running'; then
  echo "!! vm-after-failover did not reach Running after the etcd leader failover: $after_line" >&2
  echo "!! this means at least one kyuusha service's reconcile loop did not survive the failover (see docker compose logs compute-reconciler/network-reconciler/block-storage-reconciler)" >&2
  exit 1
fi
echo "    confirmed: Create -> schedule -> real Firecracker boot all still worked with the old etcd leader dead"

echo "==> confirming every reconciler container is still running (a silently-exited Watch loop would have exited its process instead of erroring -- see compute.Reconciler.Run's single, un-retried Watch call)"
for svc in compute-reconciler network-reconciler block-storage-reconciler; do
  status="$(docker inspect -f '{{.State.Status}}' "${project}-${svc}-1" 2>/dev/null || echo "missing")"
  if [ "$status" != "running" ]; then
    echo "!! ${project}-${svc}-1 is $status, not running -- its reconcile loop silently exited during the etcd failover" >&2
    exit 1
  fi
done
echo "    confirmed: all 3 reconciler containers are still running"

kill "$watch_pid" 2>/dev/null || true
wait "$watch_pid" 2>/dev/null || true
echo "==> checking whether the background 'vm watch' kept delivering events across the failover"
echo "--- watch log ---"
cat "$watch_log"
echo "--- end watch log ---"
if grep -q "$after_id" "$watch_log"; then
  echo "    confirmed: the pre-existing Watch stream, opened before the kill, delivered vm-after-failover's ($after_id) own events without needing to be re-opened"
else
  echo "!! the pre-existing Watch stream never saw vm-after-failover ($after_id) -- it silently stopped delivering events across the leader failover (client-side watch did not resume on its own)" >&2
  rm -f "$watch_log"
  exit 1
fi
rm -f "$watch_log"

echo "==> restarting the killed member ($leader) so the cluster returns to 3/3 before this script exits"
docker start "$leader" >/dev/null
for _ in $(seq 1 30); do
  health="$(docker inspect -f '{{.State.Health.Status}}' "$leader" 2>/dev/null || echo "unknown")"
  [ "$health" = "healthy" ] && break
  sleep 1
done
echo "    $leader health: ${health:-unknown}"

echo "==> ALL CHECKS PASSED: kyuusha's own etcd client/reconcile loops survived a real leader failover without any process restart"
echo "==> stack (with the 3-member etcd cluster) left running; 'docker compose -p $project down' when done"
