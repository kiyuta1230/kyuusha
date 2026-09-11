package blockstorage

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

// StorageConnectionSpec is a storage admin's declaration of which
// Availability Zones a storage backend may be connected to from --
// independent of a Volume's own spec.storage_connection (which merely
// names one of these by string, see volume.go). See the package doc
// comment and docs/open-questions.md「Hypervisor↔ストレージバックエンドの
// 接続確立をkyuusha側で自動化すべきか」「具体的な設計」for the full story.
type StorageConnectionSpec struct {
	Zones []string
	// Annotations is never interpreted by kyuusha itself -- purely a
	// reference field for admins/users (e.g. the storage product/version).
	Annotations map[string]string
}

type StorageConnectionPhase string

const (
	StorageConnectionPhasePending StorageConnectionPhase = "Pending"
	StorageConnectionPhaseReady   StorageConnectionPhase = "Ready"
)

type StorageConnectionStatus struct {
	Phase StorageConnectionPhase
	// VerifiedZones is the subset of Spec.Zones some Hypervisor has
	// actually self-reported holding this connection's name in (see
	// service.go's recordHypervisorConnections). Phase becomes Ready only
	// once this covers every declared zone -- strict, and deliberately
	// never falls back to an Error phase: a zone that's simply not
	// confirmed yet (no Hypervisor registered there yet, say) isn't a
	// failure, just not-yet (see the open-questions entry above).
	VerifiedZones []string
	Conditions    []resource.Condition
}

type StorageConnection struct {
	Meta   resource.ObjectMeta
	Spec   StorageConnectionSpec
	Status StorageConnectionStatus
}

// Delegating methods so *StorageConnection satisfies resource.Meta,
// letting it plug into the generic resource.Store. Not tenant-scoped (same
// as Hypervisor) -- Meta.TenantID is always "".
func (c *StorageConnection) GetID() string                        { return c.Meta.ID }
func (c *StorageConnection) SetID(id string)                      { c.Meta.ID = id }
func (c *StorageConnection) GetName() string                      { return c.Meta.Name }
func (c *StorageConnection) SetName(name string)                  { c.Meta.Name = name }
func (c *StorageConnection) GetTenantID() string                  { return c.Meta.TenantID }
func (c *StorageConnection) SetTenantID(id string)                { c.Meta.TenantID = id }
func (c *StorageConnection) GetResourceVersion() int64            { return c.Meta.ResourceVersion }
func (c *StorageConnection) SetResourceVersion(rv int64)          { c.Meta.ResourceVersion = rv }
func (c *StorageConnection) GetCreatedAt() time.Time              { return c.Meta.CreatedAt }
func (c *StorageConnection) SetCreatedAt(t time.Time)             { c.Meta.CreatedAt = t }
func (c *StorageConnection) GetDeletedAt() *time.Time             { return c.Meta.DeletedAt }
func (c *StorageConnection) SetDeletedAt(t *time.Time)            { c.Meta.DeletedAt = t }
func (c *StorageConnection) GetFinalizers() []resource.Finalizer  { return c.Meta.Finalizers }
func (c *StorageConnection) SetFinalizers(f []resource.Finalizer) { c.Meta.Finalizers = f }
