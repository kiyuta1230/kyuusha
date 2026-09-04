// Package compute implements the "compute" service from docs/architecture.md:
// Machine lifecycle management. This first pass covers the CRUD+Watch surface
// only; scheduling and the NATS/compute-agent side are added separately.
package compute

import "gitlab.com/ki.yuta1230/kyuusha/internal/resource"

type RecoveryPolicy string

const (
	RecoveryPolicyUnspecified RecoveryPolicy = "" // Create時にエラー(暗黙運用させない)
	RecoveryPolicyNone        RecoveryPolicy = "NONE"
	RecoveryPolicySelfHeal    RecoveryPolicy = "SELF_HEAL"
)

type VmmDriver string

const (
	VmmDriverUnspecified VmmDriver = "" // 未指定はFIRECRACKERとして扱う
	VmmDriverFirecracker VmmDriver = "FIRECRACKER"
	VmmDriverQEMU        VmmDriver = "QEMU"
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

type MachineSpec struct {
	ImageID            string
	VCPU               int32
	MemoryMB           int64
	NetworkInterfaces  []NetworkAttachment
	Volumes            []VolumeRequest
	RecoveryPolicy     RecoveryPolicy
	PersistentRootDisk bool
	UserData           string
	DriverHint         VmmDriver
	PciDevices         []PciDeviceRequest
}

type Phase string

const (
	PhasePending      Phase = "Pending"
	PhaseScheduled    Phase = "Scheduled"
	PhaseProvisioning Phase = "Provisioning"
	PhaseRunning      Phase = "Running"
	PhaseStopping     Phase = "Stopping"
	PhaseStopped      Phase = "Stopped"
	PhaseDeleting     Phase = "Deleting"
	PhaseError        Phase = "Error"
)

type MachineStatus struct {
	Phase                Phase
	Conditions           []resource.Condition
	Node                 string
	RootVolumeRef        string
	InterfaceRefs        []string
	VolumeAttachmentRefs []string
}

type Machine struct {
	Meta   resource.ObjectMeta
	Spec   MachineSpec
	Status MachineStatus
}
