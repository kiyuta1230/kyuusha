// Package image implements the "image" service from docs/architecture.md
// "imageサービスのリソース: Image": Image is metadata only (an external URL +
// digest per artifact) -- kyuusha never copies or stores image bytes
// itself. Create validates spec.format matches the artifacts actually
// provided and does a lightweight URL-reachability check; digest
// verification of the fetched bytes happens wherever a hypervisor actually
// fetches the artifact, not here.
package image

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

type Format string

const (
	FormatUnspecified  Format = ""
	FormatKernelRootfs Format = "KERNEL_ROOTFS"
	FormatQCOW2        Format = "QCOW2"
)

type Artifact struct {
	URL    string
	Digest string
}

type Spec struct {
	Format   Format
	Kernel   Artifact // KERNEL_ROOTFS only
	Rootfs   Artifact // KERNEL_ROOTFS only
	Disk     Artifact // QCOW2 only
	BootArgs string
}

type Phase string

const (
	PhasePending Phase = "Pending"
	PhaseReady   Phase = "Ready"
	PhaseError   Phase = "Error"
)

type Status struct {
	Phase      Phase
	Conditions []resource.Condition
	SizeBytes  int64
}

type Image struct {
	Meta   resource.ObjectMeta
	Spec   Spec
	Status Status
}

// Delegating methods so *Image satisfies resource.Meta, letting it plug
// into the generic resource.Store.
func (i *Image) GetID() string               { return i.Meta.ID }
func (i *Image) SetID(id string)             { i.Meta.ID = id }
func (i *Image) GetName() string             { return i.Meta.Name }
func (i *Image) SetName(name string)         { i.Meta.Name = name }
func (i *Image) GetTenantID() string         { return i.Meta.TenantID }
func (i *Image) SetTenantID(id string)       { i.Meta.TenantID = id }
func (i *Image) GetResourceVersion() int64   { return i.Meta.ResourceVersion }
func (i *Image) SetResourceVersion(rv int64) { i.Meta.ResourceVersion = rv }
func (i *Image) GetCreatedAt() time.Time     { return i.Meta.CreatedAt }
func (i *Image) SetCreatedAt(t time.Time)    { i.Meta.CreatedAt = t }
