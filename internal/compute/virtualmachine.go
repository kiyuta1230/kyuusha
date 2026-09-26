// Package compute implements the "compute" service from docs/architecture.md:
// VirtualMachine lifecycle management. This first pass covers the CRUD+Watch surface
// only; scheduling and the NATS/compute-agent side are added separately.
package compute

import (
	"fmt"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

type VmmDriver string

const (
	VmmDriverUnspecified     VmmDriver = "" // 未指定はFIRECRACKERとして扱う
	VmmDriverFirecracker     VmmDriver = "FIRECRACKER"
	VmmDriverCloudHypervisor VmmDriver = "CLOUD_HYPERVISOR"
)

// firecrackerMaxVCPU mirrors Firecracker's own MachineConfiguration.vcpu_count
// schema (1-32, and any value above 1 must be even -- Firecracker exposes
// vCPU pairs as hyperthread siblings to the guest unless vcpu_count==1).
// cloud-hypervisor's `--cpus boot=N` has no such constraint (see
// docs/specs/cloud-hypervisor-boot.md).
const firecrackerMaxVCPU = 32

// validateVCPUForDriver enforces Firecracker's vcpu_count constraint
// synchronously at Create/Resize time rather than letting it surface later
// as an opaque boot failure: fcvmm.Boot passes spec.VCPU straight into
// config.json with no local validation (internal/compute-agent/fcvmm), so
// an invalid value is only ever caught by Firecracker's own binary at
// process startup -- by which point, for Create, Hypervisor capacity has
// already been reserved and the VM has already moved through Scheduled ->
// Provisioning, or for Resize, the bad spec has already been persisted
// (Resize is synchronous and doesn't touch a VMM at all -- see Service.
// Resize) and the failure wouldn't surface until a later Start.
func validateVCPUForDriver(vcpu int32, driver VmmDriver) error {
	if driver != VmmDriverFirecracker {
		return nil
	}
	if vcpu > firecrackerMaxVCPU {
		return fmt.Errorf("%w: vcpu %d exceeds Firecracker's max of %d", ErrValidation, vcpu, firecrackerMaxVCPU)
	}
	if vcpu != 1 && vcpu%2 != 0 {
		return fmt.Errorf("%w: vcpu %d is invalid for driver_hint %s (must be 1 or an even number)", ErrValidation, vcpu, VmmDriverFirecracker)
	}
	return nil
}

// validatePciDevicesForDriver rejects spec.pci_devices on anything but
// CLOUD_HYPERVISOR: Firecracker is virtio-mmio only and has no PCI bus at
// all to attach a VFIO device to (see docs/architecture.md "PCIデバイス(GPU等)
// パススルー"), so this fails the same doomed-VM-never-created way
// validateImage's driver/format mismatch check does.
func validatePciDevicesForDriver(devices []PciDeviceRequest, driver VmmDriver) error {
	if len(devices) == 0 || driver == VmmDriverCloudHypervisor {
		return nil
	}
	return fmt.Errorf("%w: spec.pci_devices requires driver_hint %s, got %s", ErrValidation, VmmDriverCloudHypervisor, driver)
}

type NetworkAttachment struct {
	SubnetID string
	Primary  bool
}

type VolumeRequest struct {
	VolumeID   string
	DeviceHint string
}

type PciDeviceRequest struct {
	VendorID string
	DeviceID string
	Count    int32
}

type VirtualMachineSpec struct {
	ImageID           string
	VCPU              int32
	MemoryMB          int64
	NetworkInterfaces []NetworkAttachment
	Volumes           []VolumeRequest
	UserData          string
	DriverHint        VmmDriver
	PciDevices        []PciDeviceRequest
	// NumaPinned requests the scheduler pick a single host NUMA node with
	// enough spare vcpu/memory_mb and pin this VM's vCPUs/memory to it (see
	// docs/architecture.md's NUMA/CPUピニング section) -- unlike PciDevices,
	// not restricted to any particular driver_hint.
	NumaPinned bool
}

type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseScheduled    Phase = "Scheduled"
	PhaseProvisioning Phase = "Provisioning"
	PhaseRunning      Phase = "Running"
	PhaseStopping     Phase = "Stopping"
	PhaseStopped      Phase = "Stopped"
	// PhaseStarting is Stopped -> Starting -> Provisioning: a purely
	// transient phase reconcile() moves a VM straight through in the same
	// pass (see reconciler.go's provisionAndPublish), reusing the exact same
	// network/volume/boot plumbing PhaseScheduled uses -- see Service.Start.
	PhaseStarting Phase = "Starting"
	// PhaseMigrating is Stopped -> Migrating -> Scheduled -> Provisioning:
	// reconcile()'s migrateVM (reconciler.go) schedules the VM onto a
	// different Hypervisor (see hypervisor_service.go's scheduleMigration)
	// and releases the old one's capacity reservation, then hands off to
	// the exact same Scheduled-phase body a fresh Create uses -- see
	// Service.Migrate and docs/specs/virtual-machine.md「マイグレーション」.
	PhaseMigrating Phase = "Migrating"
	PhaseDeleting  Phase = "Deleting"
	PhaseError     Phase = "Error"
)

type VirtualMachineStatus struct {
	Phase                Phase
	Conditions           []resource.Condition
	Hypervisor           string
	InterfaceRefs        []string
	VolumeAttachmentRefs []string
	// StopForce carries Stop's force argument from Service.Stop through to
	// reconcile()'s PhaseStopping case (see nats.go's StopCommand) -- not
	// exposed over the wire (see grpcserver's toStatusProto/fromStatusProto):
	// it's a one-shot request parameter riding along on Status only because
	// that's the one channel reconcile()'s Watch-driven loop actually
	// observes, not a real piece of durable VM state.
	StopForce bool
	// MigrateTarget carries Migrate's optional target_hypervisor argument
	// from Service.Migrate through to reconcile()'s PhaseMigrating case
	// (migrateVM) -- same one-shot-parameter-on-Status reasoning as
	// StopForce, not exposed over the wire. Left set across a failed
	// scheduling attempt so runRetrySweep's unconditional PhaseMigrating
	// retry (same treatment as PhasePending) keeps trying the same target;
	// cleared only once migrateVM actually succeeds in scheduling
	// somewhere, so a later plain Migrate (auto-pick) on the same VM
	// doesn't inherit a stale target.
	MigrateTarget string
	// AllocatedPciDevices are the specific PCI addresses (e.g.
	// "0000:3b:00.0") scheduleVM/scheduleMigration reserved against
	// spec.pci_devices out of the current Hypervisor's self-reported
	// available_devices (see hypervisor_service.go's reservePciDevices) --
	// exposed read-only (see grpcserver's toStatusProto) so a caller can see
	// exactly which device(s) this VM got, not just what it asked for.
	// compute-agent passes these straight through as cloud-hypervisor
	// --device flags (see nats.go's CreateCommand.PciDevices).
	AllocatedPciDevices []string
	// AllocatedNumaNode is the host NUMA node id spec.numa_pinned reserved
	// this VM's vCPUs/memory against (see hypervisor_service.go's
	// reserveNumaNode) -- read-only. -1 means not pinned; proto3 has no
	// scalar "unset" of its own, so every VirtualMachineStatus literal this
	// package constructs must set this explicitly (Go's own int32 zero
	// value, 0, would otherwise be misread as "pinned to node 0").
	AllocatedNumaNode int32
}

// UnpinnedNumaNode is AllocatedNumaNode's "not pinned" sentinel -- see that
// field's doc comment.
const UnpinnedNumaNode int32 = -1

type VirtualMachine struct {
	Meta   resource.ObjectMeta
	Spec   VirtualMachineSpec
	Status VirtualMachineStatus
}

// Delegating methods so *VirtualMachine satisfies resource.Meta, letting it
// plug into the generic resource.Store. Meta is a named (not embedded)
// field so every existing vm.Meta.TenantID-style access keeps working.
func (v *VirtualMachine) GetID() string                        { return v.Meta.ID }
func (v *VirtualMachine) SetID(id string)                      { v.Meta.ID = id }
func (v *VirtualMachine) GetName() string                      { return v.Meta.Name }
func (v *VirtualMachine) SetName(name string)                  { v.Meta.Name = name }
func (v *VirtualMachine) GetTenantID() string                  { return v.Meta.TenantID }
func (v *VirtualMachine) GetCreatedAt() time.Time              { return v.Meta.CreatedAt }
func (v *VirtualMachine) SetTenantID(id string)                { v.Meta.TenantID = id }
func (v *VirtualMachine) GetResourceVersion() int64            { return v.Meta.ResourceVersion }
func (v *VirtualMachine) SetResourceVersion(rv int64)          { v.Meta.ResourceVersion = rv }
func (v *VirtualMachine) SetCreatedAt(t time.Time)             { v.Meta.CreatedAt = t }
func (v *VirtualMachine) GetDeletedAt() *time.Time             { return v.Meta.DeletedAt }
func (v *VirtualMachine) SetDeletedAt(t *time.Time)            { v.Meta.DeletedAt = t }
func (v *VirtualMachine) GetFinalizers() []resource.Finalizer  { return v.Meta.Finalizers }
func (v *VirtualMachine) SetFinalizers(f []resource.Finalizer) { v.Meta.Finalizers = f }
