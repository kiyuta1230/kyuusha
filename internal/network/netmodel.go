package network

import (
	"slices"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// See proto/kyuusha/network/v1/network.proto for what each field means;
// these are the storage-side mirrors (no proto dependency in this package,
// same convention as Subnet/NetworkInterface).

type Visibility string

const (
	VisibilityUnspecified Visibility = "" // PRIVATE
	VisibilityPrivate     Visibility = "PRIVATE"
	VisibilityPublic      Visibility = "PUBLIC"
)

// ---------------------------------------------------------------------------
// AllocationPool

type AddressBlock struct {
	CIDR      string
	GatewayIP string
}

type PoolEntry struct {
	Key        string
	Values     map[string]int64
	Addresses  []AddressBlock
	Attributes map[string]string
}

type IntRange struct{ Lo, Hi int64 }

type AddressFamily string

const (
	FamilyIPv4 AddressFamily = "IPV4"
	FamilyIPv6 AddressFamily = "IPV6"
)

type CidrMode string

const (
	CidrModeUserAny          CidrMode = "USER_ANY"
	CidrModeUserWithinBlocks CidrMode = "USER_WITHIN_BLOCKS"
	CidrModeCarve            CidrMode = "CARVE"
)

type CidrPoolSpec struct {
	Family       AddressFamily
	Mode         CidrMode
	Blocks       []string
	PrefixLength int32
}

// AllocationPoolSpec has exactly one of Entries/Integer/Cidr set.
type AllocationPoolSpec struct {
	Entries    []PoolEntry // nil unless an entries pool (an empty, non-nil slice is a valid, empty entries pool)
	Integer    []IntRange  // nil unless an integer pool
	Cidr       *CidrPoolSpec
	Attributes map[string]string
}

type PoolKind string

const (
	PoolKindEntries PoolKind = "entries"
	PoolKindInteger PoolKind = "integer"
	PoolKindCidr    PoolKind = "cidr"
)

func (s AllocationPoolSpec) Kind() PoolKind {
	switch {
	case s.Cidr != nil:
		return PoolKindCidr
	case s.Integer != nil:
		return PoolKindInteger
	default:
		return PoolKindEntries
	}
}

type AllocationPoolStatus struct {
	Allocated int32
}

type AllocationPool struct {
	Meta   resource.ObjectMeta
	Spec   AllocationPoolSpec
	Status AllocationPoolStatus
}

// ---------------------------------------------------------------------------
// NetworkClass

type PoolRef struct {
	PoolID string
	Name   string
}

type GatewayPlacement string

const (
	GatewayFirst GatewayPlacement = "FIRST"
	GatewayLast  GatewayPlacement = "LAST"
)

type NetworkClassSpec struct {
	Network               []PoolRef
	Subnet                map[string][]PoolRef // zone or "*"
	Attributes            map[string]string
	Visibility            Visibility
	SharedWithTenantIDs   []string
	AllowPublicNetworks   bool
	DefaultDNSServers     map[string][]string // zone or "*"
	MTU                   int32
	GatewayPlacement      GatewayPlacement
	HostAggregateSelector map[string]string
}

// SubnetRefs is the zone's merged Subnet-level pool list: "*" plus the
// zone's own, a zone ref replacing a "*" ref of the same Name (an unnamed
// ref -- entries/CIDR pools -- is matched by pool kind at validation time,
// not replaced here).
func (c NetworkClassSpec) SubnetRefs(zone string) []PoolRef {
	out := slices.Clone(c.Subnet["*"])
	if zone == "*" {
		return out
	}
	inherited := len(out) // only "*" refs can be replaced; a zone's own list can't override itself
	for _, r := range c.Subnet[zone] {
		replaced := false
		if r.Name != "" {
			for i := range out[:inherited] {
				if out[i].Name == r.Name {
					out[i] = r
					replaced = true
				}
			}
		}
		if !replaced {
			out = append(out, r)
		}
	}
	return out
}

// DNSServersFor is the class's default resolver list for zone.
func (c NetworkClassSpec) DNSServersFor(zone string) []string {
	if s, ok := c.DefaultDNSServers[zone]; ok {
		return s
	}
	return c.DefaultDNSServers["*"]
}

// UsableBy reports whether tenantID may create a Network of this class.
func (c NetworkClassSpec) UsableBy(tenantID string) bool {
	return c.Visibility == VisibilityPublic || slices.Contains(c.SharedWithTenantIDs, tenantID)
}

type NetworkClass struct {
	Meta resource.ObjectMeta
	Spec NetworkClassSpec
}

// ---------------------------------------------------------------------------
// Network

// Allocation is one value drawn from a pool (exactly one of Integer
// (with Name), EntryKey or CIDR is meaningful, per the pool's kind).
type Allocation struct {
	PoolID   string
	Name     string
	Integer  int64
	EntryKey string
	CIDR     string
}

type NetworkPhase string

const (
	NetworkPhasePending NetworkPhase = "Pending"
	NetworkPhaseReady   NetworkPhase = "Ready"
)

type NetworkSpec struct {
	NetworkClass        string
	DNSSuffix           string
	Visibility          Visibility
	SharedWithTenantIDs []string
}

type NetworkStatus struct {
	Phase       NetworkPhase
	Conditions  []resource.Condition
	Values      map[string]int64
	Attributes  map[string]string
	Allocations []Allocation
}

type Network struct {
	Meta   resource.ObjectMeta
	Spec   NetworkSpec
	Status NetworkStatus
}

// UsableBy reports whether tenantID may attach NetworkInterfaces to this
// Network: its owner, anyone when PUBLIC, or a tenant it's shared with.
func (n Network) UsableBy(tenantID string) bool {
	return n.Meta.TenantID == tenantID || n.Spec.Visibility == VisibilityPublic || slices.Contains(n.Spec.SharedWithTenantIDs, tenantID)
}

// ---------------------------------------------------------------------------
// resource.Meta plumbing, so all three plug into resource.Store.

func (p *AllocationPool) GetID() string                        { return p.Meta.ID }
func (p *AllocationPool) SetID(id string)                      { p.Meta.ID = id }
func (p *AllocationPool) GetName() string                      { return p.Meta.Name }
func (p *AllocationPool) SetName(name string)                  { p.Meta.Name = name }
func (p *AllocationPool) GetTenantID() string                  { return p.Meta.TenantID }
func (p *AllocationPool) SetTenantID(id string)                { p.Meta.TenantID = id }
func (p *AllocationPool) GetResourceVersion() int64            { return p.Meta.ResourceVersion }
func (p *AllocationPool) SetResourceVersion(rv int64)          { p.Meta.ResourceVersion = rv }
func (p *AllocationPool) GetCreatedAt() time.Time              { return p.Meta.CreatedAt }
func (p *AllocationPool) SetCreatedAt(t time.Time)             { p.Meta.CreatedAt = t }
func (p *AllocationPool) GetDeletedAt() *time.Time             { return p.Meta.DeletedAt }
func (p *AllocationPool) SetDeletedAt(t *time.Time)            { p.Meta.DeletedAt = t }
func (p *AllocationPool) GetFinalizers() []resource.Finalizer  { return p.Meta.Finalizers }
func (p *AllocationPool) SetFinalizers(f []resource.Finalizer) { p.Meta.Finalizers = f }

func (c *NetworkClass) GetID() string                        { return c.Meta.ID }
func (c *NetworkClass) SetID(id string)                      { c.Meta.ID = id }
func (c *NetworkClass) GetName() string                      { return c.Meta.Name }
func (c *NetworkClass) SetName(name string)                  { c.Meta.Name = name }
func (c *NetworkClass) GetTenantID() string                  { return c.Meta.TenantID }
func (c *NetworkClass) SetTenantID(id string)                { c.Meta.TenantID = id }
func (c *NetworkClass) GetResourceVersion() int64            { return c.Meta.ResourceVersion }
func (c *NetworkClass) SetResourceVersion(rv int64)          { c.Meta.ResourceVersion = rv }
func (c *NetworkClass) GetCreatedAt() time.Time              { return c.Meta.CreatedAt }
func (c *NetworkClass) SetCreatedAt(t time.Time)             { c.Meta.CreatedAt = t }
func (c *NetworkClass) GetDeletedAt() *time.Time             { return c.Meta.DeletedAt }
func (c *NetworkClass) SetDeletedAt(t *time.Time)            { c.Meta.DeletedAt = t }
func (c *NetworkClass) GetFinalizers() []resource.Finalizer  { return c.Meta.Finalizers }
func (c *NetworkClass) SetFinalizers(f []resource.Finalizer) { c.Meta.Finalizers = f }

func (n *Network) GetID() string                        { return n.Meta.ID }
func (n *Network) SetID(id string)                      { n.Meta.ID = id }
func (n *Network) GetName() string                      { return n.Meta.Name }
func (n *Network) SetName(name string)                  { n.Meta.Name = name }
func (n *Network) GetTenantID() string                  { return n.Meta.TenantID }
func (n *Network) SetTenantID(id string)                { n.Meta.TenantID = id }
func (n *Network) GetResourceVersion() int64            { return n.Meta.ResourceVersion }
func (n *Network) SetResourceVersion(rv int64)          { n.Meta.ResourceVersion = rv }
func (n *Network) GetCreatedAt() time.Time              { return n.Meta.CreatedAt }
func (n *Network) SetCreatedAt(t time.Time)             { n.Meta.CreatedAt = t }
func (n *Network) GetDeletedAt() *time.Time             { return n.Meta.DeletedAt }
func (n *Network) SetDeletedAt(t *time.Time)            { n.Meta.DeletedAt = t }
func (n *Network) GetFinalizers() []resource.Finalizer  { return n.Meta.Finalizers }
func (n *Network) SetFinalizers(f []resource.Finalizer) { n.Meta.Finalizers = f }
