// Package compute implements the "compute" service from docs/architecture.md:
// VirtualMachine lifecycle management. This first pass covers the CRUD+Watch surface
// only; scheduling and the NATS/compute-agent side are added separately.
package compute

import (
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

type VmmDriver string

const (
	VmmDriverUnspecified     VmmDriver = "" // 未指定はFIRECRACKERとして扱う
	VmmDriverFirecracker     VmmDriver = "FIRECRACKER"
	VmmDriverCloudHypervisor VmmDriver = "CLOUD_HYPERVISOR"
)

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
	PhaseDeleting Phase = "Deleting"
	PhaseError    Phase = "Error"
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
}

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
