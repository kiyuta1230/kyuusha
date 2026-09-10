package blockstorage

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

type VolumeAttachmentSpec struct {
	VolumeID   string
	VMID       string
	DeviceHint string
}

type VolumeAttachmentPhase string

const (
	VolumeAttachmentPhasePending   VolumeAttachmentPhase = "Pending"
	VolumeAttachmentPhaseAttaching VolumeAttachmentPhase = "Attaching"
	VolumeAttachmentPhaseAttached  VolumeAttachmentPhase = "Attached"
	VolumeAttachmentPhaseDetaching VolumeAttachmentPhase = "Detaching"
	VolumeAttachmentPhaseDeleting  VolumeAttachmentPhase = "Deleting"
	VolumeAttachmentPhaseError     VolumeAttachmentPhase = "Error"
)

type VolumeAttachmentStatus struct {
	Phase      VolumeAttachmentPhase
	Conditions []resource.Condition
	// DevicePath and Hypervisor stay empty: there is no report-back path
	// from compute-agent (which does know the real local device path and
	// which hypervisor it attached on) to block-storage yet -- known gap,
	// see docs/specs/volume.md "既知の未実装事項".
	DevicePath string
	Hypervisor string
}

type VolumeAttachment struct {
	Meta   resource.ObjectMeta
	Spec   VolumeAttachmentSpec
	Status VolumeAttachmentStatus
}

// Delegating methods so *VolumeAttachment satisfies resource.Meta, letting
// it plug into the generic resource.Store.
func (a *VolumeAttachment) GetID() string                        { return a.Meta.ID }
func (a *VolumeAttachment) SetID(id string)                      { a.Meta.ID = id }
func (a *VolumeAttachment) GetName() string                      { return a.Meta.Name }
func (a *VolumeAttachment) SetName(name string)                  { a.Meta.Name = name }
func (a *VolumeAttachment) GetTenantID() string                  { return a.Meta.TenantID }
func (a *VolumeAttachment) SetTenantID(id string)                { a.Meta.TenantID = id }
func (a *VolumeAttachment) GetResourceVersion() int64            { return a.Meta.ResourceVersion }
func (a *VolumeAttachment) SetResourceVersion(rv int64)          { a.Meta.ResourceVersion = rv }
func (a *VolumeAttachment) GetCreatedAt() time.Time              { return a.Meta.CreatedAt }
func (a *VolumeAttachment) SetCreatedAt(t time.Time)             { a.Meta.CreatedAt = t }
func (a *VolumeAttachment) GetDeletedAt() *time.Time             { return a.Meta.DeletedAt }
func (a *VolumeAttachment) SetDeletedAt(t *time.Time)            { a.Meta.DeletedAt = t }
func (a *VolumeAttachment) GetFinalizers() []resource.Finalizer  { return a.Meta.Finalizers }
func (a *VolumeAttachment) SetFinalizers(f []resource.Finalizer) { a.Meta.Finalizers = f }
