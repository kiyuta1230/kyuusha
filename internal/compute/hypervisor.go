package compute

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
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

// HypervisorSpec is operator intent, set only via Service.SetSchedulable --
// never touched by RegisterHypervisor, so an agent restart can't silently
// undo an operator's maintenance action. Contrast HypervisorStatus, which
// is entirely agent/heartbeat-derived.
type HypervisorSpec struct {
	Schedulable bool
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
}

type Hypervisor struct {
	Meta   resource.ObjectMeta
	Spec   HypervisorSpec
	Status HypervisorStatus
}

// Delegating methods so *Hypervisor satisfies resource.Meta, letting it
// plug into the generic resource.Store.
func (h *Hypervisor) GetID() string               { return h.Meta.ID }
func (h *Hypervisor) SetID(id string)             { h.Meta.ID = id }
func (h *Hypervisor) GetName() string             { return h.Meta.Name }
func (h *Hypervisor) SetName(name string)         { h.Meta.Name = name }
func (h *Hypervisor) GetTenantID() string         { return h.Meta.TenantID }
func (h *Hypervisor) SetTenantID(id string)       { h.Meta.TenantID = id }
func (h *Hypervisor) GetResourceVersion() int64   { return h.Meta.ResourceVersion }
func (h *Hypervisor) SetResourceVersion(rv int64) { h.Meta.ResourceVersion = rv }
func (h *Hypervisor) GetCreatedAt() time.Time     { return h.Meta.CreatedAt }
func (h *Hypervisor) SetCreatedAt(t time.Time)    { h.Meta.CreatedAt = t }
