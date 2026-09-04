// Package identity implements the "identity" service from
// docs/architecture.md: it owns Tenant, the resource every other service's
// tenant_id refers to, and the Quota limit values (usage accounting and
// enforcement stay each resource-owning service's own responsibility --
// see "Quota設計"). This first pass is CRUD+Watch only, mirroring compute's
// pattern; Create-time quota enforcement wired into compute is a follow-up.
package identity

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

type QuotaSpec struct {
	MaxVCPU          int32
	MaxMemoryMB      int64
	MaxVolumeGB      int64
	MaxVMs           int32
	MaxVCPUPerVM     int32
	MaxMemoryMBPerVM int64
}

type TenantSpec struct {
	DisplayName string
	Quota       QuotaSpec
}

type Phase string

const (
	PhaseActive   Phase = "Active"
	PhaseDeleting Phase = "Deleting"
	PhaseError    Phase = "Error"
)

type TenantStatus struct {
	Phase      Phase
	Conditions []resource.Condition
}

type Tenant struct {
	Meta   resource.ObjectMeta
	Spec   TenantSpec
	Status TenantStatus
}

// Delegating methods so *Tenant satisfies resource.Meta, letting it plug
// into the generic resource.Store. Tenant is self-referential: a Tenant's
// own Meta.TenantID equals its Meta.ID (see Service.Create), which is what
// lets a caller "self-service" Get/Watch their own Tenant through the same
// tenant-scoped authorization compute's VirtualMachine uses.
func (t *Tenant) GetID() string               { return t.Meta.ID }
func (t *Tenant) SetID(id string)             { t.Meta.ID = id }
func (t *Tenant) GetName() string             { return t.Meta.Name }
func (t *Tenant) SetName(name string)         { t.Meta.Name = name }
func (t *Tenant) GetTenantID() string         { return t.Meta.TenantID }
func (t *Tenant) SetTenantID(id string)       { t.Meta.TenantID = id }
func (t *Tenant) GetResourceVersion() int64   { return t.Meta.ResourceVersion }
func (t *Tenant) SetResourceVersion(rv int64) { t.Meta.ResourceVersion = rv }
func (t *Tenant) SetCreatedAt(tm time.Time)   { t.Meta.CreatedAt = tm }
