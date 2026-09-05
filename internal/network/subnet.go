// Package network implements the "network" service from docs/architecture.md
// "networkサービスのリソース: Subnet / NetworkInterface": Subnet and
// NetworkInterface CRUD+Watch, with real (if simple) IPAM -- Subnet Create
// allocates a VLAN ID from a per-zone pool, NetworkInterface Create
// allocates an IP from its Subnet's own CIDR (see ipam.go). There is still
// no per-hypervisor tap wiring or agent side at all (see docs/specs/network.md
// for exactly what's covered and why compute-agent, not a new network-agent,
// is expected to own tap wiring once it exists).
package network

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

type SubnetSpec struct {
	Zone                string
	CIDR                string
	GatewayIP           string
	DNSServers          []string
	SharedWithTenantIDs []string
	DNSSuffix           string
}

type SubnetPhase string

const (
	SubnetPhasePending  SubnetPhase = "Pending"
	SubnetPhaseReady    SubnetPhase = "Ready"
	SubnetPhaseDeleting SubnetPhase = "Deleting"
	SubnetPhaseError    SubnetPhase = "Error"
)

type SubnetStatus struct {
	Phase      SubnetPhase
	Conditions []resource.Condition
	VLANID     int32
}

type Subnet struct {
	Meta   resource.ObjectMeta
	Spec   SubnetSpec
	Status SubnetStatus
}

// Delegating methods so *Subnet satisfies resource.Meta, letting it plug
// into the generic resource.Store.
func (s *Subnet) GetID() string               { return s.Meta.ID }
func (s *Subnet) SetID(id string)             { s.Meta.ID = id }
func (s *Subnet) GetName() string             { return s.Meta.Name }
func (s *Subnet) SetName(name string)         { s.Meta.Name = name }
func (s *Subnet) GetTenantID() string         { return s.Meta.TenantID }
func (s *Subnet) SetTenantID(id string)       { s.Meta.TenantID = id }
func (s *Subnet) GetResourceVersion() int64   { return s.Meta.ResourceVersion }
func (s *Subnet) SetResourceVersion(rv int64) { s.Meta.ResourceVersion = rv }
func (s *Subnet) GetCreatedAt() time.Time     { return s.Meta.CreatedAt }
func (s *Subnet) SetCreatedAt(t time.Time)    { s.Meta.CreatedAt = t }
