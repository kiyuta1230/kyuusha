#!/usr/bin/env bash
# Brings up the multi-hypervisor docker-compose playground and drives it with the
# real CLI: create several VMs and confirm the (still-stub) round-robin
# scheduler actually spreads them across hypervisor-1/2/3. Leaves the stack
# running afterwards; `docker compose down` when done.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> starting docker compose stack"
docker compose up -d --build

echo "==> waiting for compute's gRPC port to accept connections"
for _ in $(seq 1 30); do
  if (exec 3<>/dev/tcp/127.0.0.1/8081) 2>/dev/null; then
    exec 3>&-
    break
  fi
  sleep 1
done

tenant="scenario-$(date +%s)"
count=6
echo "==> creating $count VMs for tenant $tenant"
for i in $(seq 1 "$count"); do
  go run ./cmd/kyuusha vm create -addr=localhost:8081 -tenant="$tenant" -name="vm-$i" \
    -image=img-scenario -vcpu=1 -memory-mb=512 -wait
done

echo "==> final state"
go run ./cmd/kyuusha vm list -addr=localhost:8081 -tenant="$tenant"

echo "==> hypervisor distribution (expect it spread across hypervisor-1/2/3)"
go run ./cmd/kyuusha vm list -addr=localhost:8081 -tenant="$tenant" \
  | grep -o 'hypervisor=[^ ]*' | sort | uniq -c

echo "==> stack left running; run 'docker compose down' when done"
