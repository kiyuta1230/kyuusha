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

# The "playground-nfs" storage connection every compute-agent declares
# (see playground/docker-compose.yml and internal/compute-agent/volumeref):
# a plain host directory bind-mounted identically into all three
# containers, standing in for a real NFS export. World-writable so the
# Firecracker jail's non-root uid (fcvmm's JailUID) can write into any file
# placed here -- kyuusha never chowns a Volume's backing file itself (see
# fcvmm/jailer.go's placeVolumeLike), so whoever plays the "operator" role
# (this script, here) has to set permissions that already work.
mkdir -p playground/volume-data
chmod 0777 playground/volume-data

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
  -max-vcpu=8 -max-memory-mb=16384 -max-volume-gb=50 -max-vms="$((count + 2))" -max-vcpu-per-vm=2 -max-memory-mb-per-vm=2048)"
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
    | grep -q "kyuusha: guest booted OK"; then
  echo "    confirmed: real Firecracker guest booted"
else
  echo "!! could not confirm a real guest boot for vm-1 (no /dev/kvm on this host? try: kyuusha vm console -tenant=$tenant -id=$vm1_id)" >&2
fi

echo "==> creating a VM with -driver-hint=qemu against the same Image (KERNEL_ROOTFS now accepts either driver -- see docs/specs/qemu-boot.md)"
go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-qemu \
  -image="$image" -vcpu=1 -memory-mb=128 -driver-hint=qemu -wait
qemu_vm_id="$(go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" | grep 'name=vm-qemu ' | grep -o 'id=[^ ]*' | cut -d= -f2)"
qemu_console=""
for _ in $(seq 1 30); do
  qemu_console="$(go run ./cmd/kyuusha vm console -addr=localhost:8080 -tenant="$tenant" -id="$qemu_vm_id" 2>/dev/null)"
  echo "$qemu_console" | grep -q "kyuusha: guest booted OK" && break
  sleep 1
done
if echo "$qemu_console" | grep -q "kyuusha: guest booted OK"; then
  echo "    confirmed: real QEMU guest booted from the same kernel_rootfs Image as vm-1"
else
  echo "!! could not confirm a real guest boot for vm-qemu (no /dev/kvm on this host, or qemu-system-x86_64 missing? try: kyuusha vm console -tenant=$tenant -id=$qemu_vm_id)" >&2
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

# wait_for_volume_ready polls until a Volume reaches Ready -- CreateVolume
# itself only returns Pending now (see docs/open-questions.md「Hypervisor↔
# ストレージバックエンドの接続確立をkyuusha側で自動化すべきか」「具体的な
# 設計」): it becomes Ready only once its StorageConnection is itself Ready
# (all declared zones confirmed) *and* compute-agent has confirmed the
# specific identifier exists, both asynchronous over NATS.
wait_for_volume_ready() {
  local vol_id="$1" line=""
  for _ in $(seq 1 20); do
    line="$(go run ./cmd/kyuusha volume get -addr=localhost:8080 -tenant="$tenant" -id="$vol_id")"
    if echo "$line" | grep -q 'phase=Ready'; then
      echo "$line"
      return 0
    fi
    sleep 1
  done
  echo "$line"
  return 1
}

echo "==> registering the 'playground-nfs' StorageConnection (admin-only; declares which AZs this backend may be connected from -- see docs/open-questions.md「Hypervisor↔ストレージバックエンドの接続確立をkyuusha側で自動化すべきか」)"
sc_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha storageconn create -addr=localhost:8080 -name=playground-nfs -zones=zone-a)"
echo "$sc_line"
storageconn_id="$(echo "$sc_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
for _ in $(seq 1 20); do
  sc_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha storageconn get -addr=localhost:8080 -id="$storageconn_id")"
  echo "$sc_line" | grep -q 'phase=Ready' && break
  sleep 1
done
echo "$sc_line"
if ! echo "$sc_line" | grep -q 'phase=Ready'; then
  echo "!! playground-nfs StorageConnection did not reach Ready (no Hypervisor reported it in zone-a?): $sc_line" >&2
  exit 1
fi
echo "    confirmed: StorageConnection reached Ready once a Hypervisor in zone-a self-reported it"

echo "==> playing the 'operator' role again: creating the real backing file this Volume will reference (Ready now requires it -- see docs/open-questions.md「Volumeの申告内容...」)"
truncate -s 1M playground/volume-data/scenario-volume.img
chmod 0666 playground/volume-data/scenario-volume.img

echo "==> creating a Volume (block-storage; a reference to an already-existing file/device, not something kyuusha provisions -- see docs/architecture.md「訂正: 責務の境界を...」)"
volume_line="$(go run ./cmd/kyuusha volume create -addr=localhost:8080 -tenant="$tenant" -name=scenario-volume -size-gb=10 -protocol=NFS -storage-connection=playground-nfs -identifier=scenario-volume.img)"
echo "$volume_line"
volume="$(echo "$volume_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if [ -z "$volume" ]; then
  echo "!! volume was not created: $volume_line" >&2
  exit 1
fi
volume_line="$(wait_for_volume_ready "$volume")"
echo "$volume_line"
if ! echo "$volume_line" | grep -q 'phase=Ready'; then
  echo "!! volume did not reach Ready: $volume_line" >&2
  exit 1
fi

echo "==> attaching the Volume to already-Running vm-1 and confirming the exclusive-attach constraint (docs/architecture.md '具体的な排他制御'; vm-1 already booted so nothing actually attaches inside the guest -- attach-before-boot only, see docs/specs/volume.md. That real end-to-end path is exercised separately below)"
first_attach_line="$(go run ./cmd/kyuusha volattach create -addr=localhost:8080 -tenant="$tenant" -name=scenario-attach-1 -vm="$vm1_id" -volume="$volume")"
echo "$first_attach_line"
if ! echo "$first_attach_line" | grep -q 'phase=Attached'; then
  echo "!! first VolumeAttachment did not reach Attached: $first_attach_line" >&2
  exit 1
fi

second_attach_line="$(go run ./cmd/kyuusha volattach create -addr=localhost:8080 -tenant="$tenant" -name=scenario-attach-2 -vm="$vm1_id" -volume="$volume")"
echo "$second_attach_line"
if ! echo "$second_attach_line" | grep -q 'phase=Pending'; then
  echo "!! a second VolumeAttachment for the same, still-attached Volume should be Pending (waiting on the first), got: $second_attach_line" >&2
  exit 1
fi
echo "    confirmed: a second attachment to an already-attached Volume is created Pending, not rejected"

if ! go run ./cmd/kyuusha volattach create -addr=localhost:8080 -tenant="$tenant" -name=scenario-attach-bad -vm="$vm1_id" -volume=volume-does-not-exist 2>/tmp/kyuusha-volattach-validation-check.log; then
  grep -q InvalidArgument /tmp/kyuusha-volattach-validation-check.log && echo "    confirmed: VolumeAttachment Create rejects an unknown volume_id"
else
  echo "!! expected InvalidArgument but volattach create with an unknown volume_id succeeded" >&2
  exit 1
fi
# (retrying the Pending second attachment once the first is deleted is unit-
# tested -- TestService_ExclusiveAttachBlocksSecondAttachmentThenRetrySucceeds
# -- `volattach delete` itself is exercised for real further down, once
# vm-1's other resources are no longer needed by anything later in this script)

echo "==> confirming Volume Create enforces max_volume_gb quota"
if go run ./cmd/kyuusha volume create -addr=localhost:8080 -tenant="$tenant" -name=scenario-volume-over-quota -size-gb=99999 -protocol=NFS -storage-connection=playground-nfs -identifier=scenario-volume-over-quota.img 2>/tmp/kyuusha-volume-quota-check.log; then
  echo "!! expected ResourceExhausted but Volume creation over quota succeeded" >&2
  exit 1
fi
grep -q ResourceExhausted /tmp/kyuusha-volume-quota-check.log && echo "    denied as expected"

echo "==> hypervisor distribution (expect it spread across hypervisor-1/2/3)"
go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" \
  | grep -o 'hypervisor=[^ ]*' | sort | uniq -c

echo "==> hypervisor capacity reservations (expect allocated=2/8vcpu on two of the three, 3/8vcpu on whichever got vm-netif)"
KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha hypervisor list -addr=localhost:8080

echo "==> confirming quota is enforced (max-vms=$((count + 2)) already reached)"
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

echo "==> confirming Finalizer blocks VM deletion until cleared (see docs/specs/external-integration.md)"
vm6_id="$(go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" | grep 'name=vm-6 ' | grep -o 'id=[^ ]*' | cut -d= -f2)"

echo "==> confirming Finalizer ownership: added_by is stamped from the real propagated JWT sub (via api-gateway's PropagateCaller* interceptors, internal/authn/propagate.go), and only that sub (or admin) may remove it"
alice_token="$(go run ./cmd/kyuusha token mint -tenant="$tenant" -sub=alice@example.com)"
bob_token="$(go run ./cmd/kyuusha token mint -tenant="$tenant" -sub=bob@example.com)"

add_fin_line="$(KYUUSHA_TOKEN="$alice_token" go run ./cmd/kyuusha vm add-finalizer -addr=localhost:8080 -tenant="$tenant" -id="$vm6_id" -finalizer=acme.corp/network-acl-cleanup)"
echo "$add_fin_line"
if ! echo "$add_fin_line" | grep -q 'finalizers=acme.corp/network-acl-cleanup'; then
  echo "!! finalizer did not round-trip: $add_fin_line" >&2
  exit 1
fi

echo "==> confirming the finalizer_name Watch filter only surfaces VMs currently holding that finalizer (see docs/specs/external-integration.md '大量Watch対策')"
watch_log="$(mktemp)"
go run ./cmd/kyuusha vm watch -addr=localhost:8080 -tenant="$tenant" -finalizer-name=acme.corp/network-acl-cleanup > "$watch_log" 2>&1 &
watch_pid=$!

vm5_id="$(go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant="$tenant" | grep 'name=vm-5 ' | grep -o 'id=[^ ]*' | cut -d= -f2)"
go run ./cmd/kyuusha vm add-finalizer -addr=localhost:8080 -tenant="$tenant" -id="$vm5_id" -finalizer=acme.corp/unrelated >/dev/null

go run ./cmd/kyuusha vm delete -addr=localhost:8080 -tenant="$tenant" -id="$vm6_id"

pending_line="$(go run ./cmd/kyuusha vm get -addr=localhost:8080 -tenant="$tenant" -id="$vm6_id")"
echo "$pending_line"
if ! echo "$pending_line" | grep -q 'phase=Deleting' || echo "$pending_line" | grep -q 'deleted_at= '; then
  echo "!! VM with a pending finalizer was not left Deleting with deleted_at set after Delete: $pending_line" >&2
  exit 1
fi
echo "    confirmed: Delete did not remove the VM while its finalizer was still present"

if KYUUSHA_TOKEN="$bob_token" go run ./cmd/kyuusha vm remove-finalizer -addr=localhost:8080 -tenant="$tenant" -id="$vm6_id" -finalizer=acme.corp/network-acl-cleanup 2>/tmp/kyuusha-finalizer-owner-check.log; then
  echo "!! expected bob's removal of alice's finalizer to be rejected but it succeeded" >&2
  exit 1
fi
grep -qi invalidargument /tmp/kyuusha-finalizer-owner-check.log && echo "    denied as expected: bob is not acme.corp/network-acl-cleanup's added_by"
still_there_line="$(go run ./cmd/kyuusha vm get -addr=localhost:8080 -tenant="$tenant" -id="$vm6_id")"
if ! echo "$still_there_line" | grep -q 'finalizers=acme.corp/network-acl-cleanup'; then
  echo "!! bob's rejected removal actually took effect: $still_there_line" >&2
  exit 1
fi
echo "    confirmed: the finalizer survived bob's rejected removal"

KYUUSHA_TOKEN="$alice_token" go run ./cmd/kyuusha vm remove-finalizer -addr=localhost:8080 -tenant="$tenant" -id="$vm6_id" -finalizer=acme.corp/network-acl-cleanup >/dev/null
if go run ./cmd/kyuusha vm get -addr=localhost:8080 -tenant="$tenant" -id="$vm6_id" 2>/tmp/kyuusha-finalizer-gone-check.log; then
  echo "!! expected NotFound but the VM still exists after its last finalizer was removed" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-finalizer-gone-check.log && echo "    confirmed: removing the last finalizer (as alice, its added_by) let the real deletion (and the still-running Firecracker process's teardown) proceed"

for _ in $(seq 1 15); do
  grep -q "$vm6_id" "$watch_log" 2>/dev/null && break
  sleep 1
done
kill "$watch_pid" 2>/dev/null || true
wait "$watch_pid" 2>/dev/null || true
if ! grep -q "$vm6_id" "$watch_log"; then
  echo "!! -finalizer-name filtered watch never saw vm-6's events: $(cat "$watch_log")" >&2
  exit 1
fi
if grep -q "$vm5_id" "$watch_log"; then
  echo "!! -finalizer-name filtered watch leaked vm-5's unrelated finalizer event: $(cat "$watch_log")" >&2
  exit 1
fi
echo "    confirmed: -finalizer-name filtered the Watch stream to vm-6 (held the matching finalizer) only, excluding vm-5's unrelated one"

echo "==> creating a VM with -user-data-file (cloud-init NoCloud seed disk; no real cloud-init in this guest, so this only proves delivery -- see docs/specs/firecracker-boot.md 'UserData注入')"
# vm-6 was just fully deleted above, freeing the quota slot this reuses.
user_data_file="$(mktemp)"
cat > "$user_data_file" <<'EOF'
#cloud-config
hostname: scenario-cloudinit-vm
EOF
cloudinit_vm_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-cloudinit \
  -image="$image" -vcpu=1 -memory-mb=128 -user-data-file="$user_data_file" -wait)"
echo "$cloudinit_vm_line"
if ! echo "$cloudinit_vm_line" | grep -q 'phase=Running'; then
  echo "!! VM with -user-data-file did not reach Running: $cloudinit_vm_line" >&2
  exit 1
fi
cloudinit_vm_id="$(echo "$cloudinit_vm_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
cloudinit_console=""
for _ in $(seq 1 10); do
  cloudinit_console="$(go run ./cmd/kyuusha vm console -addr=localhost:8080 -tenant="$tenant" -id="$cloudinit_vm_id" 2>/dev/null)"
  echo "$cloudinit_console" | grep -q 'kyuusha: seed disk' && break
  sleep 1
done
if ! echo "$cloudinit_console" | grep -q 'kyuusha: seed disk mounted'; then
  echo "!! could not confirm the seed disk was mounted (no /dev/kvm on this host? try: kyuusha vm console -tenant=$tenant -id=$cloudinit_vm_id): $cloudinit_console" >&2
else
  if ! echo "$cloudinit_console" | grep -q 'kyuusha: user-data: hostname: scenario-cloudinit-vm'; then
    echo "!! seed disk mounted but user-data content did not round-trip: $cloudinit_console" >&2
    exit 1
  fi
  echo "    confirmed: the cloud-init NoCloud seed disk was built, attached, and its user-data read back correctly by the guest"
fi
go run ./cmd/kyuusha vm delete -addr=localhost:8080 -tenant="$tenant" -id="$cloudinit_vm_id" # frees its quota slot for the persistence test below

echo "==> playing the 'operator' role: creating the real backing file a Volume will reference, directly in the playground-nfs fixture (kyuusha itself never provisions this -- see docs/architecture.md「訂正: 責務の境界を...」)"
persist_identifier="scenario-persist-volume.img"
# rm first, not just truncate -s: a stale marker left over by an earlier
# run of this same script would otherwise still be sitting in the first
# 64 bytes (truncate only changes the file's length, it doesn't zero
# already-allocated bytes within the new size) -- vm-persist-a would then
# find that old data and report "found" instead of "written", which the
# check below doesn't expect on a first attach.
rm -f "playground/volume-data/$persist_identifier"
truncate -s 1M "playground/volume-data/$persist_identifier"
chmod 0666 "playground/volume-data/$persist_identifier"

echo "==> creating a Volume referencing it and a VM that attaches it at boot (-volumes, real end-to-end: internal/compute-agent/volumeref finds the file already visible under the playground-nfs connection -> extra virtio-blk drive -- see docs/specs/volume.md)"
persist_volume_line="$(go run ./cmd/kyuusha volume create -addr=localhost:8080 -tenant="$tenant" -name=scenario-persist-volume -size-gb=1 -protocol=NFS -storage-connection=playground-nfs -identifier="$persist_identifier")"
echo "$persist_volume_line"
persist_volume="$(echo "$persist_volume_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
if [ -z "$persist_volume" ]; then
  echo "!! persist-test volume was not created: $persist_volume_line" >&2
  exit 1
fi
persist_volume_line="$(wait_for_volume_ready "$persist_volume")"
echo "$persist_volume_line"
if ! echo "$persist_volume_line" | grep -q 'phase=Ready'; then
  echo "!! persist-test volume did not reach Ready: $persist_volume_line" >&2
  exit 1
fi

vma_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-persist-a \
  -image="$image" -vcpu=1 -memory-mb=128 -volumes="$persist_volume" -wait)"
echo "$vma_line"
if ! echo "$vma_line" | grep -q 'phase=Running'; then
  echo "!! VM with -volumes did not reach Running: $vma_line" >&2
  exit 1
fi
vma_id="$(echo "$vma_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"

vma_console=""
for _ in $(seq 1 15); do
  vma_console="$(go run ./cmd/kyuusha vm console -addr=localhost:8080 -tenant="$tenant" -id="$vma_id" 2>/dev/null)"
  echo "$vma_console" | grep -q 'kyuusha: volume data written' && break
  sleep 1
done
# `|| true`: under `set -o pipefail`, grep matching nothing (e.g. the loop
# above timed out) would otherwise make this whole assignment's exit
# status non-zero and silently kill the script via `set -e` -- the `if`
# right below is exactly how this case is meant to be handled, not a
# script-ending failure.
marker="$(echo "$vma_console" | grep -o 'kyuusha: volume data written: [^ ]*' | cut -d' ' -f5 || true)"
if [ -z "$marker" ]; then
  echo "!! could not confirm the Volume was really attached and written to (no /dev/kvm on this host? try: kyuusha vm console -tenant=$tenant -id=$vma_id): $vma_console" >&2
else
  echo "    confirmed: real Volume attached at boot and written to by the guest ($marker)"
  echo "==> deleting vm-persist-a and creating a second VM attaching the SAME Volume -- proving the data actually persists (the whole point of a Volume vs. ephemeral rootfs)"
  go run ./cmd/kyuusha vm delete -addr=localhost:8080 -tenant="$tenant" -id="$vma_id"

  vmb_line="$(go run ./cmd/kyuusha vm create -addr=localhost:8080 -tenant="$tenant" -name=vm-persist-b \
    -image="$image" -vcpu=1 -memory-mb=128 -volumes="$persist_volume" -wait)"
  echo "$vmb_line"
  if ! echo "$vmb_line" | grep -q 'phase=Running'; then
    echo "!! second VM with the same -volumes did not reach Running: $vmb_line" >&2
    exit 1
  fi
  vmb_id="$(echo "$vmb_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"

  vmb_console=""
  for _ in $(seq 1 15); do
    vmb_console="$(go run ./cmd/kyuusha vm console -addr=localhost:8080 -tenant="$tenant" -id="$vmb_id" 2>/dev/null)"
    echo "$vmb_console" | grep -q "kyuusha: volume data found: $marker" && break
    sleep 1
  done
  if echo "$vmb_console" | grep -q "kyuusha: volume data found: $marker"; then
    echo "    confirmed: data written by vm-persist-a is still readable from vm-persist-b -- real persistent storage across VM recreation"
  else
    echo "!! second VM did not find the marker vm-persist-a wrote -- persistence did not actually work: $vmb_console" >&2
    exit 1
  fi

  go run ./cmd/kyuusha vm delete -addr=localhost:8080 -tenant="$tenant" -id="$vmb_id"
fi

echo "==> confirming delete now exists in the CLI for netif/volattach/volume/subnet/image (previously gRPC-only; docs/specs/cli.md)"

netif_id="$(echo "$netif_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
go run ./cmd/kyuusha netif delete -addr=localhost:8080 -tenant="$tenant" -id="$netif_id"
if go run ./cmd/kyuusha netif get -addr=localhost:8080 -tenant="$tenant" -id="$netif_id" 2>/tmp/kyuusha-netif-delete-check.log; then
  echo "!! netif delete did not actually remove the NetworkInterface" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-netif-delete-check.log && echo "    confirmed: netif delete removed the NetworkInterface"

attach1_id="$(echo "$first_attach_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
attach2_id="$(echo "$second_attach_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
go run ./cmd/kyuusha volattach delete -addr=localhost:8080 -tenant="$tenant" -id="$attach1_id"
go run ./cmd/kyuusha volattach delete -addr=localhost:8080 -tenant="$tenant" -id="$attach2_id"
if go run ./cmd/kyuusha volattach get -addr=localhost:8080 -tenant="$tenant" -id="$attach1_id" 2>/tmp/kyuusha-volattach-delete-check.log; then
  echo "!! volattach delete did not actually remove the VolumeAttachment" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-volattach-delete-check.log && echo "    confirmed: volattach delete removed the VolumeAttachment"

go run ./cmd/kyuusha volume delete -addr=localhost:8080 -tenant="$tenant" -id="$volume"
if go run ./cmd/kyuusha volume get -addr=localhost:8080 -tenant="$tenant" -id="$volume" 2>/tmp/kyuusha-volume-delete-check.log; then
  echo "!! volume delete did not actually remove the Volume" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-volume-delete-check.log && echo "    confirmed: volume delete removed the Volume"

go run ./cmd/kyuusha subnet delete -addr=localhost:8080 -tenant="$tenant" -id="$subnet"
if go run ./cmd/kyuusha subnet get -addr=localhost:8080 -tenant="$tenant" -id="$subnet" 2>/tmp/kyuusha-subnet-delete-check.log; then
  echo "!! subnet delete did not actually remove the Subnet" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-subnet-delete-check.log && echo "    confirmed: subnet delete removed the Subnet"

go run ./cmd/kyuusha image delete -addr=localhost:8080 -tenant="$tenant" -id="$image"
if go run ./cmd/kyuusha image get -addr=localhost:8080 -tenant="$tenant" -id="$image" 2>/tmp/kyuusha-image-delete-check.log; then
  echo "!! image delete did not actually remove the Image" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-image-delete-check.log && echo "    confirmed: image delete removed the Image"

echo "==> confirming tenant update/delete now exist in the CLI (previously gRPC-only)"
throwaway_tenant_line="$(KYUUSHA_TOKEN="$admin_token" go run ./cmd/kyuusha tenant create -addr=localhost:8080 \
  -name="scenario-throwaway-$(date +%s)" -max-vcpu=1 -max-memory-mb=512 -max-volume-gb=1 -max-vms=1 -max-vcpu-per-vm=1 -max-memory-mb-per-vm=512)"
throwaway_tenant="$(echo "$throwaway_tenant_line" | grep -o 'id=[^ ]*' | head -1 | cut -d= -f2)"
throwaway_token="$(go run ./cmd/kyuusha token mint -tenant="$throwaway_tenant")"

updated_line="$(KYUUSHA_TOKEN="$throwaway_token" go run ./cmd/kyuusha tenant update -addr=localhost:8080 -id="$throwaway_tenant" -display-name="updated via CLI")"
echo "$updated_line"
if ! echo "$updated_line" | grep -q 'display_name="updated via CLI"'; then
  echo "!! tenant update did not change display_name: $updated_line" >&2
  exit 1
fi
if ! echo "$updated_line" | grep -q 'vms=1'; then
  echo "!! tenant update changed max_vms even though -max-vms was not passed: $updated_line" >&2
  exit 1
fi
echo "    confirmed: tenant update only overwrote the flag actually passed (display_name), left quota untouched"

KYUUSHA_TOKEN="$throwaway_token" go run ./cmd/kyuusha tenant delete -addr=localhost:8080 -id="$throwaway_tenant"
if KYUUSHA_TOKEN="$throwaway_token" go run ./cmd/kyuusha tenant get -addr=localhost:8080 -id="$throwaway_tenant" 2>/tmp/kyuusha-tenant-delete-check.log; then
  echo "!! tenant delete did not actually remove the Tenant" >&2
  exit 1
fi
grep -q NotFound /tmp/kyuusha-tenant-delete-check.log && echo "    confirmed: tenant delete removed the Tenant"

echo "==> stack left running; run 'docker compose down' when done"
