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

// Visibility controls cross-tenant read access (Get/List/Watch, and being
// referenced by another tenant's VM at Create time -- see
// docs/specs/image.md "マルチテナント対応（可視性/共有）"). Mutation
// (SetVisibility) and Delete always remain restricted to the owning
// tenant_id regardless of Visibility: sharing only ever grants read/
// reference access, never write.
type Visibility string

const (
	VisibilityUnspecified Visibility = "" // defaults to PRIVATE at Create time
	VisibilityPrivate     Visibility = "PRIVATE"
	VisibilityPublic      Visibility = "PUBLIC"
)

type Spec struct {
	Format     Format
	Kernel     Artifact // KERNEL_ROOTFS only
	Rootfs     Artifact // KERNEL_ROOTFS only
	Disk       Artifact // QCOW2 only
	BootArgs   string
	Visibility Visibility
	// SharedWithTenantIDs is meaningful only when Visibility is
	// VisibilityPrivate: those tenants (in addition to the owner) may
	// Get/List/Watch/reference this Image, exactly as if it were their
	// own -- but never Delete or SetVisibility it. Ignored when
	// Visibility is VisibilityPublic (everyone already can see it).
	SharedWithTenantIDs []string
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
func (i *Image) GetDeletedAt() *time.Time    { return i.Meta.DeletedAt }
func (i *Image) SetDeletedAt(t *time.Time)   { i.Meta.DeletedAt = t }
func (i *Image) GetFinalizers() []string     { return i.Meta.Finalizers }
func (i *Image) SetFinalizers(f []string)    { i.Meta.Finalizers = f }
