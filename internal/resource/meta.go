// Package resource holds the types shared by every top-level kyuusha
// resource (VirtualMachine, Tenant, ...), mirroring docs/architecture.md's
// "リソース共通の型" section, plus the generic Store implementing the
// common Create/Get/List/Update/Delete/Watch pattern every service's
// resource API follows.
package resource

import "time"

// ObjectMeta is embedded in every top-level resource.
type ObjectMeta struct {
	ID              string
	Name            string
	TenantID        string
	ResourceVersion int64
	CreatedAt       time.Time
	DeletedAt       *time.Time // nil unless Delete has been called; see Finalizers
	// Finalizers holds opaque holder names; a non-empty list blocks Store.
	// Delete from actually removing the object (see store.go's Delete/
	// Update and docs/architecture.md "Finalizer"). Almost always empty in
	// practice today -- only VirtualMachine actively uses it so far.
	Finalizers []string
}

func (m *ObjectMeta) GetID() string              { return m.ID }
func (m *ObjectMeta) SetID(id string)            { m.ID = id }
func (m *ObjectMeta) GetName() string            { return m.Name }
func (m *ObjectMeta) SetName(name string)        { m.Name = name }
func (m *ObjectMeta) GetTenantID() string        { return m.TenantID }
func (m *ObjectMeta) SetTenantID(id string)      { m.TenantID = id }
func (m *ObjectMeta) GetResourceVersion() int64  { return m.ResourceVersion }
func (m *ObjectMeta) SetResourceVersion(v int64) { m.ResourceVersion = v }
func (m *ObjectMeta) GetCreatedAt() time.Time    { return m.CreatedAt }
func (m *ObjectMeta) SetCreatedAt(t time.Time)   { m.CreatedAt = t }
func (m *ObjectMeta) GetDeletedAt() *time.Time   { return m.DeletedAt }
func (m *ObjectMeta) SetDeletedAt(t *time.Time)  { m.DeletedAt = t }
func (m *ObjectMeta) GetFinalizers() []string    { return m.Finalizers }
func (m *ObjectMeta) SetFinalizers(f []string)   { m.Finalizers = f }

// Meta is the constraint every generic Store[T, PT] resource type's pointer
// receiver (PT) must satisfy. Resources embed ObjectMeta as a named field
// (not anonymously, so call sites keep writing vm.Meta.TenantID etc.) and
// add small delegating methods to satisfy this interface -- see
// VirtualMachine's or Tenant's *.go for the pattern.
type Meta interface {
	GetID() string
	SetID(string)
	GetName() string
	SetName(string)
	GetTenantID() string
	SetTenantID(string)
	GetResourceVersion() int64
	SetResourceVersion(int64)
	GetCreatedAt() time.Time
	SetCreatedAt(time.Time)
	GetDeletedAt() *time.Time
	SetDeletedAt(*time.Time)
	GetFinalizers() []string
	SetFinalizers([]string)
}
