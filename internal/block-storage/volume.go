// Package blockstorage implements the "block-storage" service from
// docs/architecture.md "block-storageサービスのリソース: Volume /
// VolumeAttachment": Volume/VolumeAttachment CRUD+Watch, Quota (max_volume_gb)
// enforcement, and the exclusive-attach constraint that guards against
// double-attaching a Volume (see docs/architecture.md "未解決の危険:
// フェンシング問題"). kyuusha does not provision or export storage itself
// (see docs/architecture.md「訂正: 責務の境界を...」) -- a Volume is a
// reference to a block device or file that already exists and is already
// reachable from whichever Hypervisors declared the matching
// StorageConnection at registration. See docs/specs/volume.md for the full
// story.
package blockstorage

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

type StorageProtocol string

const (
	StorageProtocolISCSI  StorageProtocol = "ISCSI"
	StorageProtocolNVMeOF StorageProtocol = "NVME_OF"
	StorageProtocolNFS    StorageProtocol = "NFS"
)

type VolumeSpec struct {
	SizeGB   int64 // self-reported (kyuusha never provisions, so it can't verify this) -- used for quota only
	Protocol StorageProtocol
	// StorageConnection names the Hypervisor-side connection this Volume
	// lives behind -- must match a name a Hypervisor declared in its own
	// StorageConnections at registration (docs/specs/hypervisor-bootstrap.md).
	StorageConnection string
	// Identifier is protocol-specific: for ISCSI/NVMeOF, the block device's
	// stable serial/WWN (as seen under /dev/disk/by-id/ once the
	// Hypervisor's StorageConnection session makes it visible); for NFS, a
	// file path relative to wherever that Hypervisor mounted the connection.
	Identifier string
	// Annotations is never interpreted by kyuusha itself -- purely a
	// reference field for admins/users (e.g. a QoS tier).
	Annotations map[string]string
}

type VolumePhase string

const (
	VolumePhasePending  VolumePhase = "Pending"
	VolumePhaseReady    VolumePhase = "Ready"
	VolumePhaseDeleting VolumePhase = "Deleting"
	VolumePhaseError    VolumePhase = "Error"
)

type VolumeStatus struct {
	Phase      VolumePhase
	Conditions []resource.Condition
}

type Volume struct {
	Meta   resource.ObjectMeta
	Spec   VolumeSpec
	Status VolumeStatus
}

// Delegating methods so *Volume satisfies resource.Meta, letting it plug
// into the generic resource.Store.
func (v *Volume) GetID() string                        { return v.Meta.ID }
func (v *Volume) SetID(id string)                      { v.Meta.ID = id }
func (v *Volume) GetName() string                      { return v.Meta.Name }
func (v *Volume) SetName(name string)                  { v.Meta.Name = name }
func (v *Volume) GetTenantID() string                  { return v.Meta.TenantID }
func (v *Volume) SetTenantID(id string)                { v.Meta.TenantID = id }
func (v *Volume) GetResourceVersion() int64            { return v.Meta.ResourceVersion }
func (v *Volume) SetResourceVersion(rv int64)          { v.Meta.ResourceVersion = rv }
func (v *Volume) GetCreatedAt() time.Time              { return v.Meta.CreatedAt }
func (v *Volume) SetCreatedAt(t time.Time)             { v.Meta.CreatedAt = t }
func (v *Volume) GetDeletedAt() *time.Time             { return v.Meta.DeletedAt }
func (v *Volume) SetDeletedAt(t *time.Time)            { v.Meta.DeletedAt = t }
func (v *Volume) GetFinalizers() []resource.Finalizer  { return v.Meta.Finalizers }
func (v *Volume) SetFinalizers(f []resource.Finalizer) { v.Meta.Finalizers = f }
