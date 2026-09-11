package compute

import (
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// See docs/architecture.md "computeサービスのリソース: Hypervisor": an
// internal management object for scheduling, not a KaaS-facing resource.
// Not tenant-scoped -- Meta.TenantID is always "" -- and Meta.ID is the
// operator/agent-chosen hypervisor string (e.g. "hypervisor-1") already
// used throughout the NATS subjects, not a minted id.

type HypervisorPhase string

const (
	HypervisorPhaseReady    HypervisorPhase = "Ready"
	HypervisorPhaseNotReady HypervisorPhase = "NotReady"
)

type PciDevice struct {
	PCIAddress string
	VendorID   string
	DeviceID   string
	Allocated  bool
}

// StorageConnection is one storage connection this Hypervisor self-reports
// as already established (an iSCSI/NVMe-oF session already logged in, or
// an NFS export already mounted -- see docs/architecture.md「訂正: 責務の
// 境界を...」). Static self-report only: RegisterHypervisor stores whatever
// compute-agent claims at startup and never re-verifies it here -- see
// docs/open-questions.md「Hypervisorのストレージ接続自己申告を動的化すべきか」.
type StorageConnection struct {
	Name      string
	LocalPath string
}

// HypervisorSpec is operator intent, set only via Service.SetSchedulable/
// SetRevoked -- never touched by RegisterHypervisor, so an agent restart
// can't silently undo an operator's maintenance action. Contrast
// HypervisorStatus, which is entirely agent/heartbeat-derived.
type HypervisorSpec struct {
	Schedulable bool
	// Revoked blocks this hypervisor id from ever registering again
	// (RegisterHypervisor rejects it outright) -- for a decommissioned or
	// compromised host. See Service.SetRevoked's own doc comment for the
	// deliberate scope boundary (it blocks future registration only, not
	// already-flowing east-west traffic).
	Revoked bool
}

type HypervisorStatus struct {
	Phase               HypervisorPhase
	Zone                string
	LastHeartbeatAt     time.Time
	AllocatableVCPU     int32
	AllocatableMemoryMB int64
	AllocatedVCPU       int32
	AllocatedMemoryMB   int64
	SupportedDrivers    []string
	AvailableDevices    []PciDevice
	// StorageConnections is this Hypervisor's self-reported set of already-
	// established storage connections -- see the StorageConnection type's
	// own doc comment. block-storage's own StorageConnection resource
	// verification (docs/open-questions.md) learns about these via a NATS
	// event the Reconciler publishes whenever this changes, not by
	// block-storage dialing compute directly (kept one-way: compute already
	// depends on block-storage for Volume validation).
	StorageConnections []StorageConnection
}

type Hypervisor struct {
	Meta   resource.ObjectMeta
	Spec   HypervisorSpec
	Status HypervisorStatus
}

// Delegating methods so *Hypervisor satisfies resource.Meta, letting it
// plug into the generic resource.Store.
func (h *Hypervisor) GetID() string                        { return h.Meta.ID }
func (h *Hypervisor) SetID(id string)                      { h.Meta.ID = id }
func (h *Hypervisor) GetName() string                      { return h.Meta.Name }
func (h *Hypervisor) SetName(name string)                  { h.Meta.Name = name }
func (h *Hypervisor) GetTenantID() string                  { return h.Meta.TenantID }
func (h *Hypervisor) SetTenantID(id string)                { h.Meta.TenantID = id }
func (h *Hypervisor) GetResourceVersion() int64            { return h.Meta.ResourceVersion }
func (h *Hypervisor) SetResourceVersion(rv int64)          { h.Meta.ResourceVersion = rv }
func (h *Hypervisor) GetCreatedAt() time.Time              { return h.Meta.CreatedAt }
func (h *Hypervisor) SetCreatedAt(t time.Time)             { h.Meta.CreatedAt = t }
func (h *Hypervisor) GetDeletedAt() *time.Time             { return h.Meta.DeletedAt }
func (h *Hypervisor) SetDeletedAt(t *time.Time)            { h.Meta.DeletedAt = t }
func (h *Hypervisor) GetFinalizers() []resource.Finalizer  { return h.Meta.Finalizers }
func (h *Hypervisor) SetFinalizers(f []resource.Finalizer) { h.Meta.Finalizers = f }
