// Package blockstorage implements the "block-storage" service from
// docs/architecture.md "block-storageサービスのリソース: Volume /
// VolumeAttachment": Volume/VolumeAttachment CRUD+Watch, Quota (max_volume_gb)
// enforcement, and the exclusive-attach constraint that guards against
// double-attaching a Volume (see docs/architecture.md "未解決の危険:
// フェンシング問題"). There is no real StorageBackend yet -- see
// docs/specs/volume.md for exactly what this covers and why compute-agent
// doesn't touch iSCSI/NVMe-oF at all yet.
package blockstorage

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

type VolumeSpec struct {
	SizeGB int64
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
func (v *Volume) GetID() string               { return v.Meta.ID }
func (v *Volume) SetID(id string)             { v.Meta.ID = id }
func (v *Volume) GetName() string             { return v.Meta.Name }
func (v *Volume) SetName(name string)         { v.Meta.Name = name }
func (v *Volume) GetTenantID() string         { return v.Meta.TenantID }
func (v *Volume) SetTenantID(id string)       { v.Meta.TenantID = id }
func (v *Volume) GetResourceVersion() int64   { return v.Meta.ResourceVersion }
func (v *Volume) SetResourceVersion(rv int64) { v.Meta.ResourceVersion = rv }
func (v *Volume) GetCreatedAt() time.Time     { return v.Meta.CreatedAt }
func (v *Volume) SetCreatedAt(t time.Time)    { v.Meta.CreatedAt = t }
