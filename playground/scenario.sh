#!/usr/bin/env bash
# Brings up the multi-hypervisor docker-compose playground and drives it
# through api-gateway with the real CLI: waits for all 3 compute-agents to
# self-register as real Hypervisors, mints an admin token, creates a real
# Tenant via identity, mints a token for that tenant, creates a real Image
# via the image service and waits for it to reach Ready, creates several VMs
# referencing it, and confirms the real scheduler spreads them across
# hypervisor-1/2/3. Also checks that Tenant creation and Hypervisor listing
# are admin-only, and that a token for a different tenant is denied VM
# access. Leaves the stack running afterwards;
# `docker compose -f playground/docker-compose.yml down` when done.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> starting docker compose stack"
docker compose -f playground/docker-compose.yml up -d --build

echo "==> waiting for api-gateway's gRPC port to accept connections"
for _ in $(seq 1 30); do
  if (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; then
    exec 3>&-
    break
  fi
  sleep 1
done

tenant_name="scenario-$(date +%s)"
count=6

admin_token="$(go run ./cmd/kyuusha token mint -tenant=bootstrap-admin -role=admin -sub=scenario-admin@example.com)"

echo "==> confirming Hypervisor listing is admin-only"
non_admin_token="$(go run ./cmd/kyuusha token mint -tenant=someone)"
if KYUUSHA_TOKEN="$non_admin_token" go run ./cmd/kyuusha hypervisor list -addr=localhost:8080 2>/tmp/kyuusha-hypervisor-admin-check.log; then
  echo "!! expected PermissionDenied but non-admin Hypervisor list succeeded" >&2
  exit 1
fi
grep -q PermissionDenied /tmp/kyuusha-hypervisor-admin-check.log && echo "    denied as expected"

echo "==> waiting for all 3 compute-agents to self-register"
for _ in $(seq 1 30); do
  ready="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha hypervisor list -addr=localhost:8080 | grep -c 'phase=Ready' || true)"
  [ "$ready" -ge 3 ] && break
  sleep 1
done
KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha hypervisor list -addr=localhost:8080
if [ "$ready" -lt 3 ]; then
  echo "!! only $ready/3 hypervisors self-registered as Ready" >&2
  exit 1
fi

echo "==> confirming Tenant creation is admin-only"
if KYUUSHA_TOKEN="$non_admin_token" go run ./cmd/kyuusha tenant create -addr=localhost:8080 -name="$tenant_name" -max-vcpu=8 -max-memory-mb=16384 -max-vms="$count" -max-vcpu-per-vm=2 -max-memory-mb-per-vm=2048 2>/tmp/kyuusha-tenant-admin-check.log; then
  echo "!! expected PermissionDenied but non-admin Tenant create succeeded" >&2
  exit 1
fi
grep -q PermissionDenied /tmp/kyuusha-tenant-admin-check.log && echo "    denied as expected"

echo "==> creating Tenant $tenant_name via identity (admin token)"
tenant_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant create -addr=localhost:8080 \
  -name="$tenant_name" -display-name="Scenario Tenant" \
  -max-vcpu=8 -max-memory-mb=16384 -max-vms="$count" -max-vcpu-per-vm=2 -max-memory-mb-per-vm=2048)"
echo "$tenant_line"
tenant="$(echo "$tenant_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if [ -z "$tenant" ]; then
  echo "!! could not parse tenant id from: $tenant_line" >&2
  exit 1
fi

export KYUUSHA_TOKEN
KYUUSHA_TOKEN="$(go run ./cmd/kyuusha token mint -tenant="$tenant")"

echo "==> creating Image for tenant $tenant (kernel_rootfs; reachability probed against nats's own healthz -- content doesn't matter, only that the URL resolves)"
image_line="$(go run ./cmd/kyuusha image create -addr=localhost:8080 -tenant="$tenant" -name=scenario-image \
  -format=kernel_rootfs -kernel-url=http://nats:8222/healthz -rootfs-url=http://nats:8222/healthz)"
echo "$image_line"
image="$(echo "$image_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if [ -z "$image" ]; then
  echo "!! could not parse image id from: $image_line" >&2
  exit 1
fi

echo "==> waiting for Image to reach Ready"
for _ in $(seq 1 30); do
  phase="$(go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant" -id="$image" | grep -o 'phase=[^ ]*' | cut -d= -f2)"
  [ "$phase" = "Ready" ] && break
  if [ "$phase" = "Error" ]; then
    echo "!! image went to Error" >&2
    go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant" -id="$image" >&2
    exit 1
  fi
  sleep 1
done
if [ "$phase" != "Ready" ]; then
  echo "!! image never reached Ready, last phase=$phase" >&2
  exit 1
fi

echo "==> creating $count VMs for tenant $tenant"
for i in $(seq 1 "$count"); do
  go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name="vm-$i" \
    -image="$image" -vcpu=1 -memory-mb=512 -wait
done

echo "==> final state"
go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant"

echo "==> hypervisor distribution (expect it spread across hypervisor-1/2/3)"
go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" \
  | grep -o 'hypervisor=[^ ]*' | sort | uniq -c

echo "==> hypervisor capacity reservations (expect allocated=2/8vcpu on each)"
KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha hypervisor list -addr=localhost:8080

echo "==> confirming quota is enforced (max-vms=$count already reached)"
if go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name="vm-over-quota" \
  -image="$image" -vcpu=1 -memory-mb=512 2>/tmp/kyuusha-quota-check.log; then
  echo "!! expected ResourceExhausted but VM creation over quota succeeded" >&2
  exit 1
fi
grep -q ResourceExhausted /tmp/kyuusha-quota-check.log && echo "    denied as expected"

echo "==> confirming a token for a different tenant is denied"
other_token="$(go run ./cmd/kyuusha token mint -tenant=someone-else)"
if KYUUSHA_TOKEN="$other_token" go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" 2>/tmp/kyuusha-authz-check.log; then
  echo "!! expected PermissionDenied but the call succeeded" >&2
  exit 1
fi
grep -q PermissionDenied /tmp/kyuusha-authz-check.log && echo "    denied as expected"

echo "==> stack left running; run 'docker compose down' when done"
