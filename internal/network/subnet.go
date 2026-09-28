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
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

type SubnetSpec struct {
	Zone       string
	CIDR       string
	GatewayIP  string
	DNSServers []string
	DNSSuffix  string
	// MeshGroup declares intent only (see docs/specs/network.md): Subnets
	// sharing a non-empty MeshGroup (same tenant only) are meant to
	// default-allow each other, bypassing the normal cross-Subnet deny.
	// Nothing enforces this yet -- no ACL engine exists, the same stage
	// IngressRules itself is in.
	MeshGroup string
	// AllocatableIPRanges restricts IPAM to these "<start>-<end>" IPv4
	// ranges instead of the whole CIDR (see ipam.go); empty means the
	// default (whole CIDR minus network/broadcast/GatewayIP).
	AllocatableIPRanges []string
	// UniqueCidr declares that this Subnet's CIDR must not overlap any
	// other Subnet (any tenant) that also has UniqueCidr true -- checked at
	// Create/Update time (see validateUniqueCIDR). false (the default)
	// keeps today's behavior: CIDR overlap across different
	// mesh_groups/tenants is fine (different VRF/VLAN). Independent of
	// Visibility -- see docs/specs/network.md.
	UniqueCidr bool
	// Visibility and SharedWithTenantIDs mirror image.Visibility/
	// image.Spec.SharedWithTenantIDs exactly: who besides the owning
	// tenant may actually attach a NetworkInterface to this Subnet (see
	// subnetUsableBy). Not the same thing field 5 used to be (that was
	// ACL-reference-only consent, removed -- see the proto's own comment).
	Visibility          SubnetVisibility
	SharedWithTenantIDs []string
}

type SubnetVisibility string

const (
	SubnetVisibilityUnspecified SubnetVisibility = "" // defaults to PRIVATE at Create time
	SubnetVisibilityPrivate     SubnetVisibility = "PRIVATE"
	SubnetVisibilityPublic      SubnetVisibility = "PUBLIC"
)

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

// subnetUsableBy mirrors image.visibleTo exactly: true if tenantID may
// attach a NetworkInterface to sn -- sn's own owner, a PUBLIC sn (any
// tenant), or a PRIVATE sn that explicitly names tenantID in
// shared_with_tenant_ids.
func subnetUsableBy(sn Subnet, tenantID string) bool {
	if sn.Meta.TenantID == tenantID {
		return true
	}
	if sn.Spec.Visibility == SubnetVisibilityPublic {
		return true
	}
	return slices.Contains(sn.Spec.SharedWithTenantIDs, tenantID)
}

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

// validateUniqueCIDR implements SubnetSpec.unique_cidr: when unique, cidr
// must not overlap any other Subnet (any tenant, including the same one --
// unlike shared_with_tenant_ids's same-tenant exemption, two of your own
// unique_cidr Subnets overlapping is still wrong) that also has unique_cidr
// set. excludeSubnetID skips a Subnet's own prior record on Update. Reuses
// cidrsOverlap (firewallrule.go), the same overlap check
// validateCrossTenantRules used to.
func (s *Service) validateUniqueCIDR(ctx context.Context, excludeSubnetID, cidr string) error {
	_, cidrNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return err // already rejected by the caller's own CIDR parse
	}
	all, err := s.subnets.List(ctx, "")
	if err != nil {
		return err
	}
	for _, sn := range all {
		if sn.Meta.ID == excludeSubnetID || !sn.Spec.UniqueCidr {
			continue
		}
		_, snNet, err := net.ParseCIDR(sn.Spec.CIDR)
		if err != nil {
			continue
		}
		if cidrsOverlap(cidrNet, snNet) {
			return fmt.Errorf("%w: cidr %q overlaps unique_cidr Subnet %q (tenant %q, cidr %q)",
				ErrValidation, cidr, sn.Meta.ID, sn.Meta.TenantID, sn.Spec.CIDR)
		}
	}
	return nil
}
