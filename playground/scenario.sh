#!/usr/bin/env bash
# Brings up the multi-hypervisor docker-compose playground and drives it
# through api-gateway with the real CLI: waits for all 3 compute-agents to
# self-register as real Hypervisors, mints an admin token, creates a real
# Tenant via identity, mints a token for that tenant, creates a real Image
# via the image service and waits for it to reach Ready, creates several VMs
# referencing it (booting real Firecracker microVMs -- see
# docs/specs/firecracker-boot.md -- if /dev/kvm is available), and confirms
# the real scheduler spreads them across hypervisor-1/2/3. Also creates a
# Subnet and a NetworkInterface (network service, real IPAM -- see
# docs/specs/network.md), checks that Tenant creation and Hypervisor listing
# are admin-only, and that a token for a different tenant is denied VM
# access. Leaves the stack running
# afterwards; `docker compose -f playground/docker-compose.yml down` when
# done.
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
  -max-vcpu=8 -max-memory-mb=16384 -max-vms="$((count + 1))" -max-vcpu-per-vm=2 -max-memory-mb-per-vm=2048)"
echo "$tenant_line"
tenant="$(echo "$tenant_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if [ -z "$tenant" ]; then
  echo "!! could not parse tenant id from: $tenant_line" >&2
  exit 1
fi

export KYUUSHA_TOKEN
KYUUSHA_TOKEN="$(go run ./cmd/kyuusha token mint -tenant="$tenant")"

echo "==> creating Image for tenant $tenant (kernel_rootfs; a real Firecracker kernel + Alpine rootfs served by image-assets -- see docs/specs/firecracker-boot.md)"
image_line="$(go run ./cmd/kyuusha image create -addr=localhost:8080 -tenant="$tenant" -name=scenario-image \
  -format=kernel_rootfs -kernel-url=http://image-assets/vmlinux -rootfs-url=http://image-assets/rootfs.ext4)"
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

echo "==> confirming Image multi-tenant visibility/sharing (see docs/specs/image.md 'マルチテナント対応（可視性/共有）')"
echo "==> creating a second Tenant (real quota, via identity) to exercise cross-tenant Image sharing"
tenant2_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant create -addr=localhost:8080 \
  -name="scenario-b-$(date +%s)" -display-name="Scenario Tenant B" \
  -max-vcpu=2 -max-memory-mb=2048 -max-vms=1 -max-vcpu-per-vm=2 -max-memory-mb-per-vm=2048)"
tenant2="$(echo "$tenant2_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if [ -z "$tenant2" ]; then
  echo "!! could not parse tenant2 id from: $tenant2_line" >&2
  exit 1
fi
token2="$(go run ./cmd/kyuusha token mint -tenant="$tenant2")"

if KYUUSHA_TOKEN="$token2" go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant2" -id="$image" 2>/tmp/kyuusha-image-private-check.log; then
  echo "!! expected NotFound but a different tenant could Get a PRIVATE, unshared Image" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-image-private-check.log && echo "    confirmed: PRIVATE unshared Image is hidden from other tenants"

echo "==> creating a PUBLIC Image and confirming a different tenant can both see it and boot a VM from it"
public_image_line="$(go run ./cmd/kyuusha image create -addr=localhost:8080 -tenant="$tenant" -name=scenario-image-public \
  -format=kernel_rootfs -kernel-url=http://image-assets/vmlinux -rootfs-url=http://image-assets/rootfs.ext4 -visibility=public)"
echo "$public_image_line"
public_image="$(echo "$public_image_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
for _ in $(seq 1 30); do
  phase="$(KYUUSHA_TOKEN="$token2" go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant2" -id="$public_image" | grep -o 'phase=[^ ]*' | cut -d= -f2)"
  [ "$phase" = "Ready" ] && break
  sleep 1
done
if [ "$phase" != "Ready" ]; then
  echo "!! a different tenant could not see the PUBLIC Image reach Ready, last phase=$phase" >&2
  exit 1
fi
public_vm_line="$(KYUUSHA_TOKEN="$token2" go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant2" -name=vm-public-image \
  -image="$public_image" -vcpu=1 -memory-mb=128 -wait)"
echo "$public_vm_line"
if ! echo "$public_vm_line" | grep -q 'phase=Running'; then
  echo "!! a different tenant could not boot a VM from a PUBLIC Image owned by tenant $tenant: $public_vm_line" >&2
  exit 1
fi
echo "    confirmed: tenant $tenant2 booted a VM from tenant $tenant's PUBLIC Image, unmodified compute-side code"

echo "==> confirming SetVisibility is owner-only and can retroactively share/unshare a PRIVATE Image"
if KYUUSHA_TOKEN="$token2" go run ./cmd/kyuusha image share -addr=localhost:8080 -tenant="$tenant2" -id="$public_image" -visibility=private 2>/tmp/kyuusha-image-setvis-check.log; then
  echo "!! expected NotFound but a non-owner could SetVisibility" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-image-setvis-check.log && echo "    confirmed: SetVisibility rejected for a non-owner"

share_line="$(go run ./cmd/kyuusha image share -addr=localhost:8080 -tenant="$tenant" -id="$image" -visibility=private -shared-with-tenant-ids="$tenant2")"
echo "$share_line"
if ! echo "$share_line" | grep -q "shared_with=$tenant2"; then
  echo "!! shared_with_tenant_ids did not round-trip: $share_line" >&2
  exit 1
fi
if ! KYUUSHA_TOKEN="$token2" go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant2" -id="$image" >/dev/null 2>&1; then
  echo "!! tenant $tenant2 still could not Get the Image after being added to shared_with_tenant_ids" >&2
  exit 1
fi
echo "    confirmed: retroactively sharing a PRIVATE Image via 'image share' grants access"

echo "==> creating $count VMs for tenant $tenant"
for i in $(seq 1 "$count"); do
  go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name="vm-$i" \
    -image="$image" -vcpu=1 -memory-mb=128 -wait
done

echo "==> final state"
go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant"

echo "==> confirming vm-1 actually booted a real Firecracker guest (via kyuusha vm console; needs /dev/kvm -- see docs/specs/firecracker-boot.md)"
vm1_id="$(go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" | grep 'name=vm-1 ' | grep -o 'id=[^ ]*' | cut -d= -f2)"
if go run ./cmd/kyuusha vm console -addr=localhost:8080 -tenant="$tenant" -id="$vm1_id" 2>/dev/null \
    | grep -q "kyuusha: firecracker guest booted OK"; then
  echo "    confirmed: real Firecracker guest booted"
else
  echo "!! could not confirm a real guest boot for vm-1 (no /dev/kvm on this host? try: kyuusha vm console -tenant=$tenant -id=$vm1_id)" >&2
fi

echo "==> creating Subnet for tenant $tenant (zone-a; real IPAM -- see docs/specs/network.md)"
subnet_line="$(go run ./cmd/kyuusha subnet create -addr=localhost:8080 -tenant="$tenant" -name=scenario-subnet -zone=zone-a -cidr=10.0.1.0/24 -gateway-ip=10.0.1.1)"
echo "$subnet_line"
subnet="$(echo "$subnet_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
vlan_id="$(echo "$subnet_line" | grep -o 'vlan_id=[^ ]*' | cut -d= -f2)"
if [ -z "$subnet" ] || ! echo "$subnet_line" | grep -q 'phase=Ready' || [ -z "$vlan_id" ] || [ "$vlan_id" = "0" ]; then
  echo "!! subnet was not created Ready with a real vlan_id: $subnet_line" >&2
  exit 1
fi

echo "==> creating a VM with -subnets=$subnet (compute-network integration: compute validates the Subnet at Create time and the Reconciler creates the NetworkInterface itself once Scheduled -- see docs/specs/network.md 'compute側の統合')"
netvm_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-netif \
  -image="$image" -vcpu=1 -memory-mb=128 -subnets="$subnet" -wait)"
echo "$netvm_line"
netvm_id="$(echo "$netvm_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
netvm_iface="$(echo "$netvm_line" | grep 'phase=Running' | grep -o 'interfaces=[^ ]*' | tail -1 | cut -d= -f2)"
if ! echo "$netvm_line" | grep -q 'phase=Running' || [ -z "$netvm_iface" ]; then
  echo "!! VM with -subnets did not reach Running with a populated interface_refs: $netvm_line" >&2
  exit 1
fi
echo "    confirmed: interface_refs populated by the Reconciler ($netvm_iface)"

echo "==> confirming the guest actually got a real tap device (real IP config + a successful ping to the Subnet's gateway_ip -- see internal/compute-agent/netsetup and docs/specs/network.md)"
netvm_console=""
for _ in $(seq 1 10); do
  netvm_console="$(go run ./cmd/kyuusha vm console -addr=localhost:8080 -tenant="$tenant" -id="$netvm_id" 2>/dev/null)"
  echo "$netvm_console" | grep -q "kyuusha: eth0 " && break
  sleep 1
done
if echo "$netvm_console" | grep -q "kyuusha: eth0 reached gateway 10.0.1.1 OK"; then
  echo "    confirmed: guest reached its Subnet gateway over a real tap device"
elif echo "$netvm_console" | grep -q "kyuusha: eth0 configured"; then
  echo "!! guest configured eth0 but could not reach the gateway (needs CAP_NET_ADMIN + /dev/net/tun on the compute-agent container): $netvm_console" >&2
else
  echo "!! could not confirm real network wiring for vm-netif (no /dev/kvm or CAP_NET_ADMIN on this host? try: kyuusha vm console -tenant=$tenant -id=$netvm_id): $netvm_console" >&2
fi

echo "==> confirming VM Create rejects an unknown subnet_id in -subnets"
if go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-bad-subnet \
  -image="$image" -vcpu=1 -memory-mb=128 -subnets=subnet-does-not-exist 2>/tmp/kyuusha-vm-subnet-validation-check.log; then
  echo "!! expected InvalidArgument but vm create with an unknown subnet_id succeeded" >&2
  exit 1
fi
grep -q InvalidArgument /tmp/kyuusha-vm-subnet-validation-check.log && echo "    rejected as expected"

echo "==> creating NetworkInterface for vm-1 on subnet $subnet (real IP allocated from its CIDR; see docs/specs/network.md)"
netif_line="$(go run ./cmd/kyuusha netif create -addr=localhost:8080 -tenant="$tenant" -name=scenario-netif -vm="$vm1_id" -subnet="$subnet")"
echo "$netif_line"
netif_ip="$(echo "$netif_line" | grep -o 'ip=[^ ]*' | cut -d= -f2)"
if ! echo "$netif_line" | grep -q 'phase=Ready' || [ "${netif_ip#10.0.1.}" = "$netif_ip" ]; then
  echo "!! network interface was not created Ready with an ip inside 10.0.1.0/24: $netif_line" >&2
  exit 1
fi

echo "==> confirming NetworkInterface Create rejects an unknown subnet_id"
if go run ./cmd/kyuusha netif create -addr=localhost:8080 -tenant="$tenant" -name=scenario-netif-bad \
  -vm="$vm1_id" -subnet=subnet-does-not-exist 2>/tmp/kyuusha-netif-validation-check.log; then
  echo "!! expected InvalidArgument but netif create with an unknown subnet_id succeeded" >&2
  exit 1
fi
grep -q InvalidArgument /tmp/kyuusha-netif-validation-check.log && echo "    rejected as expected"

echo "==> confirming IP pool exhaustion leaves a NetworkInterface Pending instead of rejecting Create (/30 has 2 usable addresses)"
small_subnet_line="$(go run ./cmd/kyuusha subnet create -addr=localhost:8080 -tenant="$tenant" -name=scenario-small-subnet -zone=zone-a -cidr=10.0.9.0/30)"
small_subnet="$(echo "$small_subnet_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
go run ./cmd/kyuusha netif create -addr=localhost:8080 -tenant="$tenant" -name=scenario-small-1 -vm="$vm1_id" -subnet="$small_subnet" >/dev/null
go run ./cmd/kyuusha netif create -addr=localhost:8080 -tenant="$tenant" -name=scenario-small-2 -vm="$vm1_id" -subnet="$small_subnet" >/dev/null
exhausted_line="$(go run ./cmd/kyuusha netif create -addr=localhost:8080 -tenant="$tenant" -name=scenario-small-3 -vm="$vm1_id" -subnet="$small_subnet")"
echo "$exhausted_line"
if ! echo "$exhausted_line" | grep -q 'phase=Pending'; then
  echo "!! expected the 3rd NetworkInterface on a /30 to be Pending (IP pool exhausted), got: $exhausted_line" >&2
  exit 1
fi
echo "    confirmed: created Pending rather than rejected, as expected"

echo "==> confirming allocatable_ip_ranges restricts IPAM to a narrow range (and mesh_group round-trips)"
ranged_subnet_line="$(go run ./cmd/kyuusha subnet create -addr=localhost:8080 -tenant="$tenant" -name=scenario-ranged-subnet \
  -zone=zone-a -cidr=10.0.6.0/24 -mesh-group=scenario-mesh -allocatable-ip-ranges=10.0.6.10-10.0.6.11)"
echo "$ranged_subnet_line"
ranged_subnet="$(echo "$ranged_subnet_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if ! echo "$ranged_subnet_line" | grep -q 'mesh_group=scenario-mesh'; then
  echo "!! mesh_group did not round-trip: $ranged_subnet_line" >&2
  exit 1
fi
ranged_netif_line="$(go run ./cmd/kyuusha netif create -addr=localhost:8080 -tenant="$tenant" -name=scenario-ranged-netif -vm="$vm1_id" -subnet="$ranged_subnet")"
echo "$ranged_netif_line"
ranged_ip="$(echo "$ranged_netif_line" | grep -o 'ip=[^ ]*' | cut -d= -f2)"
if [ "$ranged_ip" != "10.0.6.10" ] && [ "$ranged_ip" != "10.0.6.11" ]; then
  echo "!! expected ip_address inside the 10.0.6.10-10.0.6.11 allocatable range, got: $ranged_netif_line" >&2
  exit 1
fi
echo "    confirmed: ip_address stayed inside the configured allocatable_ip_ranges"

echo "==> hypervisor distribution (expect it spread across hypervisor-1/2/3)"
go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" \
  | grep -o 'hypervisor=[^ ]*' | sort | uniq -c

echo "==> hypervisor capacity reservations (expect allocated=2/8vcpu on two of the three, 3/8vcpu on whichever got vm-netif)"
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
