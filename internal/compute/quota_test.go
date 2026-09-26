package compute

import (
	"context"
	"errors"
	"testing"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestService_CreateRejectsUnknownTenant(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	_, err := svc.Create(ctx, "", "x", VirtualMachineSpec{ImageID: "img-abc"})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("empty tenant_id: got %v, want ErrValidation", err)
	}
}

func TestService_CreateEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu:          3,
		MaxMemoryMb:      8192,
		MaxVms:           2,
		MaxVcpuPerVm:     2,
		MaxMemoryMbPerVm: 4096,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	// Exceeds max_vcpu_per_vm even though tenant totals have room.
	if _, err := svc.Create(ctx, tenant, "", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 4, MemoryMB: 1024,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over per-VM cap: got %v, want ErrQuotaExceeded", err)
	}

	first, err := svc.Create(ctx, tenant, "vm-1", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 2, MemoryMB: 4096,
	})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Tenant total vcpu would go to 4 > max_vcpu=3.
	if _, err := svc.Create(ctx, tenant, "vm-2", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 2, MemoryMB: 2048,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over tenant total: got %v, want ErrQuotaExceeded", err)
	}

	// A second VM that fits should still succeed.
	second, err := svc.Create(ctx, tenant, "vm-3", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 1, MemoryMB: 1024,
	})
	if err != nil {
		t.Fatalf("second Create (within remaining quota): %v", err)
	}

	// Idempotent re-Create of the first VM must not re-charge quota (a third
	// distinct VM would otherwise now fit: used vcpu=3 == max_vcpu=3, no
	// room; but re-Create of an existing name must succeed regardless).
	again, err := svc.Create(ctx, tenant, "vm-1", VirtualMachineSpec{ImageID: "img-abc"})
	if err != nil {
		t.Fatalf("idempotent re-Create: %v", err)
	}
	if again.Meta.ID != first.Meta.ID {
		t.Fatalf("idempotent re-Create minted a new ID: %s vs %s", again.Meta.ID, first.Meta.ID)
	}

	// max_vms=2 already reached (vm-1, vm-3); a third distinct VM must be
	// rejected even though vcpu/memory alone would fit.
	if _, err := svc.Create(ctx, tenant, "vm-4", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 0, MemoryMB: 0,
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over max_vms: got %v, want ErrQuotaExceeded", err)
	}

	// Deleting one VM frees enough quota (vcpu and vm count) for another.
	if err := svc.Delete(ctx, tenant, second.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Create(ctx, tenant, "vm-5", VirtualMachineSpec{ImageID: "img-abc",
		VCPU: 1, MemoryMB: 1024,
	}); err != nil {
		t.Fatalf("Create after Delete freed quota: %v", err)
	}
}

// TestService_ResizeEnforcesQuota exercises allow_resize's aggregate-delta
// check: growing past the tenant's remaining headroom is rejected, and
// rejection leaves both tenant_usage and the stored VM's Spec untouched (no
// partial charge).
func TestService_ResizeEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu: 4, MaxMemoryMb: 8192, MaxVms: 10, MaxVcpuPerVm: 8, MaxMemoryMbPerVm: 8192,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 16, 32768, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048})

	// Growing to vcpu=6 would push tenant total to 6 > max_vcpu=4.
	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 6, 2048); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over tenant total: got %v, want ErrQuotaExceeded", err)
	}

	usage := svc.usage[tenant]
	if usage.VCPU != 2 || usage.MemoryMB != 2048 {
		t.Fatalf("usage changed after a rejected Resize: %+v, want unchanged vcpu=2 memory_mb=2048", usage)
	}
	stored, err := svc.Get(ctx, tenant, vm.Meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Spec.VCPU != 2 || stored.Spec.MemoryMB != 2048 {
		t.Fatalf("stored Spec changed after a rejected Resize: %+v, want unchanged vcpu=2 memory_mb=2048", stored.Spec)
	}
}

// TestService_ResizeAllowsShrinkEvenNearQuotaLimit confirms a shrink is
// never blocked by quota, even when the tenant is already at its limit --
// see Service.Resize's "pure shrink never violates quota" short-circuit.
func TestService_ResizeAllowsShrinkEvenNearQuotaLimit(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu: 2, MaxMemoryMb: 2048, MaxVms: 10, MaxVcpuPerVm: 2, MaxMemoryMbPerVm: 2048,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 16, 32768, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048})

	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 1, 1024); err != nil {
		t.Fatalf("shrink at quota limit: %v", err)
	}
}

// TestService_ResizeEnforcesPerVMCap exercises allow_resize's per-VM branch
// specifically: aggregate tenant usage has headroom, but the requested new
// size alone exceeds max_vcpu_per_vm/max_memory_mb_per_vm.
func TestService_ResizeEnforcesPerVMCap(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu: 16, MaxMemoryMb: 32768, MaxVms: 10, MaxVcpuPerVm: 4, MaxMemoryMbPerVm: 4096,
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	vm := stoppedVMWithHypervisor(t, ctx, svc, tenant, "hypervisor-1", 16, 32768, VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 2048})

	if _, err := svc.Resize(ctx, tenant, vm.Meta.ID, 6, 2048); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over max_vcpu_per_vm (plenty of tenant headroom): got %v, want ErrQuotaExceeded", err)
	}
}

// TestService_CreateEnforcesPciDeviceQuota exercises quota.rego's per-
// (vendor_id, device_id) branch: a device type absent from the tenant's
// QuotaSpec.pci_devices is rejected outright (implicit max_count=0, not
// unlimited -- see PciDeviceQuota's doc comment), a device type present but
// already at its max_count is rejected, and Delete frees enough usage for a
// later Create of the same device type to succeed.
func TestService_CreateEnforcesPciDeviceQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{
		MaxVcpu: 32, MaxMemoryMb: 65536, MaxVms: 10, MaxVcpuPerVm: 32, MaxMemoryMbPerVm: 65536,
		PciDevices: []*identityv1.PciDeviceQuota{
			{VendorId: "10de", DeviceId: "1c03", MaxCount: 1},
		},
	}}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	// vendor/device not present in the tenant's quota at all.
	if _, err := svc.Create(ctx, tenant, "vm-unquoted", VirtualMachineSpec{
		ImageID: "img-abc", DriverHint: VmmDriverCloudHypervisor,
		PciDevices: []PciDeviceRequest{{VendorID: "8086", DeviceID: "1521"}},
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("device type absent from quota: got %v, want ErrQuotaExceeded", err)
	}

	first, err := svc.Create(ctx, tenant, "vm-1", VirtualMachineSpec{
		ImageID: "img-abc", DriverHint: VmmDriverCloudHypervisor,
		PciDevices: []PciDeviceRequest{{VendorID: "10de", DeviceID: "1c03"}},
	})
	if err != nil {
		t.Fatalf("first Create (within max_count=1): %v", err)
	}

	// max_count=1 already used by vm-1.
	if _, err := svc.Create(ctx, tenant, "vm-2", VirtualMachineSpec{
		ImageID: "img-abc", DriverHint: VmmDriverCloudHypervisor,
		PciDevices: []PciDeviceRequest{{VendorID: "10de", DeviceID: "1c03"}},
	}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over max_count: got %v, want ErrQuotaExceeded", err)
	}

	if err := svc.Delete(ctx, tenant, first.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Create(ctx, tenant, "vm-3", VirtualMachineSpec{
		ImageID: "img-abc", DriverHint: VmmDriverCloudHypervisor,
		PciDevices: []PciDeviceRequest{{VendorID: "10de", DeviceID: "1c03"}},
	}); err != nil {
		t.Fatalf("Create after Delete freed the device quota: %v", err)
	}
}

// TestService_CreateRejectsPciDevicesForNonCloudHypervisorDriver exercises
// validatePciDevicesForDriver: Firecracker is virtio-mmio only and has no
// PCI bus, so spec.pci_devices must be rejected before quota/scheduling ever
// runs, the same doomed-VM-never-created reasoning as validateVCPUForDriver.
func TestService_CreateRejectsPciDevicesForNonCloudHypervisorDriver(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	if _, err := svc.Create(ctx, "tenant-a", "vm-1", VirtualMachineSpec{
		ImageID: "img-abc", DriverHint: VmmDriverFirecracker,
		PciDevices: []PciDeviceRequest{{VendorID: "10de", DeviceID: "1c03"}},
	}); !errors.Is(err, ErrValidation) {
		t.Fatalf("pci_devices under FIRECRACKER: got %v, want ErrValidation", err)
	}
}

// TestService_NewServiceRebuildsUsageFromExistingVirtualMachines proves the
// real bug found 2026-09-13 (same class as
// network.Service.rebuildPools/blockstorage.Service.rebuildUsage, and
// present here even before this session's compute-reconciler split):
// usage is purely in-memory, so without rebuildUsage, a restart forgets
// every tenant's real quota usage and could let Create approve a request
// a live tenant_usage would have rejected. Constructs two Services against
// the SAME etcd client/namespace (not resourcetest.Client(t) called
// twice, which would give each its own isolated namespace) to simulate a
// real restart.
func TestService_NewServiceRebuildsUsageFromExistingVirtualMachines(t *testing.T) {
	ctx := context.Background()
	etcdClient := resourcetest.Client(t)
	quota := &identityv1.QuotaSpec{
		MaxVcpu: 4, MaxMemoryMb: 8192, MaxVms: 10, MaxVcpuPerVm: 4, MaxMemoryMbPerVm: 8192,
	}
	const tenant = "tenant-a"

	svc1, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService (first): %v", err)
	}
	// DriverHint: CloudHypervisor -- odd vcpu=3 would otherwise be rejected
	// by Firecracker's own even-or-1 vcpu_count constraint (see
	// validateVCPUForDriver); this test only cares about usage arithmetic.
	if _, err := svc1.Create(ctx, tenant, "vm-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 3, MemoryMB: 4096, DriverHint: VmmDriverCloudHypervisor}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc2, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService (second, simulating a restart): %v", err)
	}

	// max_vcpu=4, vm-1 already used 3 -- only 1 more vcpu should fit.
	// Without rebuildUsage, svc2 would start from an empty usage map and
	// wrongly allow this.
	if _, err := svc2.Create(ctx, tenant, "vm-2", VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Create (after restart, over remaining vcpu headroom): got %v, want ErrQuotaExceeded -- rebuildUsage did not restore usage's state", err)
	}
	if _, err := svc2.Create(ctx, tenant, "vm-3", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 1024}); err != nil {
		t.Fatalf("Create (after restart, within remaining vcpu headroom): %v", err)
	}
}
