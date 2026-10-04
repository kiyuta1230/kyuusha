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
	"context"
	"errors"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// SubnetAddress is one address family's CIDR and gateway.
type SubnetAddress struct {
	CIDR      string
	GatewayIP string
}

// SubnetSpec is the user-written half of a Subnet: what is being asked
// for. Everything kyuusha allocates (addresses, named values, attributes)
// lives in SubnetStatus, which Update never changes.
type SubnetSpec struct {
	NetworkID string // required, immutable
	Zone      string // required, immutable
	// RequestedAddresses is only for a NetworkClass whose CIDR pool is
	// user-specified (USER_ANY/USER_WITHIN_BLOCKS); immutable.
	RequestedAddresses []SubnetAddress
	// DNSServers empty means the NetworkClass's default for Zone.
	DNSServers []string
	// AllocatableIPRanges restricts IPAM to these "<start>-<end>" IPv4
	// ranges instead of the whole CIDR (see ipam.go); empty means the
	// default (whole CIDR minus network/broadcast/gateway).
	AllocatableIPRanges []string
}

type SubnetPhase string

const (
	SubnetPhasePending  SubnetPhase = "Pending"
	SubnetPhaseReady    SubnetPhase = "Ready"
	SubnetPhaseDeleting SubnetPhase = "Deleting"
	SubnetPhaseError    SubnetPhase = "Error"
)

type SubnetStatus struct {
	Phase       SubnetPhase
	Conditions  []resource.Condition
	Addresses   []SubnetAddress // effective, at most one per address family
	Values      map[string]int64
	Attributes  map[string]string
	Allocations []Allocation
}

// IPv4 returns the Subnet's effective IPv4 CIDR and gateway ("" until
// allocated) -- the only family kyuusha's IPAM hands addresses out of.
func (s SubnetStatus) IPv4() (cidr, gatewayIP string) {
	for _, a := range s.Addresses {
		if familyOf(a.CIDR) == FamilyIPv4 {
			return a.CIDR, a.GatewayIP
		}
	}
	return "", ""
}

type Subnet struct {
	Meta   resource.ObjectMeta
	Spec   SubnetSpec
	Status SubnetStatus
}

// Delegating methods so *Subnet satisfies resource.Meta, letting it plug
// into the generic resource.Store.
func (s *Subnet) GetID() string                        { return s.Meta.ID }
func (s *Subnet) SetID(id string)                      { s.Meta.ID = id }
func (s *Subnet) GetName() string                      { return s.Meta.Name }
func (s *Subnet) SetName(name string)                  { s.Meta.Name = name }
func (s *Subnet) GetTenantID() string                  { return s.Meta.TenantID }
func (s *Subnet) SetTenantID(id string)                { s.Meta.TenantID = id }
func (s *Subnet) GetResourceVersion() int64            { return s.Meta.ResourceVersion }
func (s *Subnet) SetResourceVersion(rv int64)          { s.Meta.ResourceVersion = rv }
func (s *Subnet) GetCreatedAt() time.Time              { return s.Meta.CreatedAt }
func (s *Subnet) SetCreatedAt(t time.Time)             { s.Meta.CreatedAt = t }
func (s *Subnet) GetDeletedAt() *time.Time             { return s.Meta.DeletedAt }
func (s *Subnet) SetDeletedAt(t *time.Time)            { s.Meta.DeletedAt = t }
func (s *Subnet) GetFinalizers() []resource.Finalizer  { return s.Meta.Finalizers }
func (s *Subnet) SetFinalizers(f []resource.Finalizer) { s.Meta.Finalizers = f }

// getSubnetForInterface resolves subnetID for a NetworkInterface Create or
// enforcement pass, allowing cross-tenant resolution (unlike a plain
// s.subnets.Get(ctx, tenantID, subnetID), which is scoped to tenantID's own
// namespace): tenantID's own Subnets resolve on the fast path; anything
// else falls back to a full scan (mirrors block-storage's
// hasActiveAttachment: fine at this system's target scale). This function
// never checks subnetUsableBy itself -- only CreateNetworkInterface does,
// at attach time -- so an already-attached NetworkInterface keeps
// resolving its Subnet even if visibility/shared_with_tenant_ids changes
// afterward.
func (s *Service) getSubnetForInterface(ctx context.Context, tenantID, subnetID string) (Subnet, error) {
	if sn, err := s.subnets.Get(ctx, tenantID, subnetID); err == nil {
		return sn, nil
	} else if !errors.Is(err, ErrSubnetNotFound) {
		return Subnet{}, err
	}
	all, err := s.subnets.List(ctx, "")
	if err != nil {
		return Subnet{}, err
	}
	for _, sn := range all {
		if sn.Meta.ID == subnetID {
			return sn, nil
		}
	}
	return Subnet{}, ErrSubnetNotFound
}
