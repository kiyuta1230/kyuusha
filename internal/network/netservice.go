package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// ---------------------------------------------------------------------------
// AllocationPool (cluster-scoped; authz lets only cross-tenant roles reach
// these RPCs, since none of them carries a tenant_id)

func (s *Service) CreateAllocationPool(ctx context.Context, name string, spec AllocationPoolSpec, md resource.Metadata) (*AllocationPool, error) {
	if err := resource.ValidateMetadata(md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if err := validatePoolSpec(spec); err != nil {
		return nil, err
	}
	out, err := s.pools.Create(ctx, "", name, AllocationPool{
		Meta: resource.ObjectMeta{Labels: md.Labels, Annotations: md.Annotations},
		Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	return s.withPoolUsage(ctx, out)
}

func (s *Service) GetAllocationPool(ctx context.Context, id string) (*AllocationPool, error) {
	p, err := s.pools.Get(ctx, "", id)
	if err != nil {
		return nil, err
	}
	return s.withPoolUsage(ctx, p)
}

func (s *Service) ListAllocationPools(ctx context.Context) ([]AllocationPool, error) {
	pools, err := s.pools.List(ctx, "")
	if err != nil {
		return nil, err
	}
	usage, err := s.poolAllocations(ctx)
	if err != nil {
		return nil, err
	}
	for i := range pools {
		pools[i].Status.Allocated = int32(len(usage[pools[i].Meta.ID]))
	}
	return pools, nil
}

func (s *Service) WatchAllocationPools(ctx context.Context, sinceRV int64) (<-chan resource.Event[AllocationPool], error) {
	return s.pools.Watch(ctx, "", sinceRV, nil)
}

// UpdateAllocationPool replaces a pool's spec, refusing any change that
// would strand a live allocation (a range, entry or block it came from).
func (s *Service) UpdateAllocationPool(ctx context.Context, pool *AllocationPool) (*AllocationPool, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: pool.Meta.Labels, Annotations: pool.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := validatePoolSpec(pool.Spec); err != nil {
		return nil, err
	}
	current, err := s.pools.Get(ctx, "", pool.Meta.ID)
	if err != nil {
		return nil, err
	}
	if current.Spec.Kind() != pool.Spec.Kind() {
		return nil, fmt.Errorf("%w: a pool's kind (entries/integer/cidr) can't change", ErrValidation)
	}
	usage, err := s.poolAllocations(ctx)
	if err != nil {
		return nil, err
	}
	for _, al := range usage[pool.Meta.ID] {
		if !poolStillCovers(pool.Spec, al) {
			return nil, fmt.Errorf("%w: pool %q still has %s allocated, which the new spec no longer covers", ErrInUse, pool.Meta.Name, describeAllocation(al))
		}
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, pool.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	pool.Meta.Finalizers = finalizers
	pool.Status = current.Status
	out, err := s.pools.Update(ctx, *pool)
	if err != nil {
		return nil, err
	}
	return s.withPoolUsage(ctx, out)
}

func (s *Service) DeleteAllocationPool(ctx context.Context, id string) error {
	usage, err := s.poolAllocations(ctx)
	if err != nil {
		return err
	}
	if n := len(usage[id]); n > 0 {
		return fmt.Errorf("%w: %d values are still allocated from pool %q", ErrInUse, n, id)
	}
	classes, err := s.classes.List(ctx, "")
	if err != nil {
		return err
	}
	for _, c := range classes {
		for _, ref := range allRefs(c.Spec) {
			if ref.PoolID == id {
				return fmt.Errorf("%w: NetworkClass %q still references pool %q", ErrInUse, c.Meta.Name, id)
			}
		}
	}
	return s.pools.Delete(ctx, "", id)
}

func (s *Service) withPoolUsage(ctx context.Context, p AllocationPool) (*AllocationPool, error) {
	usage, err := s.poolAllocations(ctx)
	if err != nil {
		return nil, err
	}
	p.Status.Allocated = int32(len(usage[p.Meta.ID]))
	return &p, nil
}

// poolAllocations gathers every live allocation, by pool id, from every
// Network's and Subnet's status (the durable record; the allocator is only
// network-reconciler's in-memory mirror of it).
func (s *Service) poolAllocations(ctx context.Context) (map[string][]Allocation, error) {
	out := map[string][]Allocation{}
	networks, err := s.networks.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, n := range networks {
		for _, al := range n.Status.Allocations {
			out[al.PoolID] = append(out[al.PoolID], al)
		}
	}
	subnets, err := s.subnets.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, sn := range subnets {
		for _, al := range sn.Status.Allocations {
			out[al.PoolID] = append(out[al.PoolID], al)
		}
	}
	return out, nil
}

func poolStillCovers(spec AllocationPoolSpec, al Allocation) bool {
	switch spec.Kind() {
	case PoolKindInteger:
		for _, r := range spec.Integer {
			if al.Integer >= r.Lo && al.Integer <= r.Hi {
				return true
			}
		}
		return false
	case PoolKindEntries:
		return slices.ContainsFunc(spec.Entries, func(e PoolEntry) bool { return e.Key == al.EntryKey })
	default:
		if spec.Cidr.Mode == CidrModeUserAny {
			return true
		}
		_, n, err := net.ParseCIDR(al.CIDR)
		return err == nil && insideAny(n, spec.Cidr.Blocks)
	}
}

func describeAllocation(al Allocation) string {
	switch {
	case al.EntryKey != "":
		return fmt.Sprintf("entry %q", al.EntryKey)
	case al.CIDR != "":
		return al.CIDR
	default:
		return fmt.Sprintf("%s=%d", al.Name, al.Integer)
	}
}

func allRefs(c NetworkClassSpec) []PoolRef {
	out := slices.Clone(c.Network)
	for _, refs := range c.Subnet {
		out = append(out, refs...)
	}
	return out
}

func (s *Service) poolsByID(ctx context.Context) (map[string]AllocationPool, error) {
	pools, err := s.pools.List(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]AllocationPool, len(pools))
	for _, p := range pools {
		out[p.Meta.ID] = p
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// NetworkClass (cluster-scoped; a tenant may read the classes it can use)

func (s *Service) CreateNetworkClass(ctx context.Context, name string, spec NetworkClassSpec, md resource.Metadata) (*NetworkClass, error) {
	if err := resource.ValidateMetadata(md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	pools, err := s.poolsByID(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateClassSpec(spec, pools); err != nil {
		return nil, err
	}
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "CREATE", Resource: "NetworkClass", Name: name,
		Labels: md.Labels, Annotations: md.Annotations, Spec: admissionClassSpecJSON(spec),
	}); err != nil {
		return nil, err
	}
	out, err := s.classes.Create(ctx, "", name, NetworkClass{
		Meta: resource.ObjectMeta{Labels: md.Labels, Annotations: md.Annotations},
		Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetNetworkClass with tenantID empty returns any class; with tenantID
// set, only one that tenant may use (anything else reads as NotFound).
func (s *Service) GetNetworkClass(ctx context.Context, tenantID, id string) (*NetworkClass, error) {
	c, err := s.classes.Get(ctx, "", id)
	if err != nil {
		return nil, err
	}
	if tenantID != "" && !c.Spec.UsableBy(tenantID) {
		return nil, ErrNetworkClassNotFound
	}
	return &c, nil
}

func (s *Service) ListNetworkClasses(ctx context.Context, tenantID string) ([]NetworkClass, error) {
	all, err := s.classes.List(ctx, "")
	if err != nil || tenantID == "" {
		return all, err
	}
	out := all[:0]
	for _, c := range all {
		if c.Spec.UsableBy(tenantID) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *Service) WatchNetworkClasses(ctx context.Context, sinceRV int64) (<-chan resource.Event[NetworkClass], error) {
	return s.classes.Watch(ctx, "", sinceRV, nil)
}

// UpdateNetworkClass replaces a class's spec. Classes are referenced, not
// copied: the change affects later allocations only.
func (s *Service) UpdateNetworkClass(ctx context.Context, class *NetworkClass) (*NetworkClass, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: class.Meta.Labels, Annotations: class.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	pools, err := s.poolsByID(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateClassSpec(class.Spec, pools); err != nil {
		return nil, err
	}
	current, err := s.classes.Get(ctx, "", class.Meta.ID)
	if err != nil {
		return nil, err
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, class.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	class.Meta.Finalizers = finalizers
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "UPDATE", Resource: "NetworkClass", Name: current.Meta.Name, ID: current.Meta.ID,
		Labels: class.Meta.Labels, Annotations: class.Meta.Annotations, Spec: admissionClassSpecJSON(class.Spec),
		OldObject: admissionClassObject(current),
	}); err != nil {
		return nil, err
	}
	out, err := s.classes.Update(ctx, *class)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) DeleteNetworkClass(ctx context.Context, id string) error {
	current, err := s.classes.Get(ctx, "", id)
	if err != nil {
		return err
	}
	networks, err := s.networks.List(ctx, "")
	if err != nil {
		return err
	}
	for _, n := range networks {
		if n.Spec.NetworkClass == id {
			return fmt.Errorf("%w: Network %q (tenant %q) still uses class %q", ErrInUse, n.Meta.Name, n.Meta.TenantID, current.Meta.Name)
		}
	}
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "DELETE", Resource: "NetworkClass", Name: current.Meta.Name, ID: id,
		OldObject: admissionClassObject(current),
	}); err != nil {
		return err
	}
	return s.classes.Delete(ctx, "", id)
}

// ---------------------------------------------------------------------------
// Network

func (s *Service) CreateNetwork(ctx context.Context, tenantID, name string, spec NetworkSpec, md resource.Metadata) (*Network, error) {
	if err := resource.ValidateMetadata(md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if existing, ok := s.networks.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}
	if spec.NetworkClass == "" {
		return nil, fmt.Errorf("%w: spec.network_class is required (there is no default class)", ErrValidation)
	}
	class, err := s.classes.Get(ctx, "", spec.NetworkClass)
	if err != nil {
		if errors.Is(err, ErrNetworkClassNotFound) {
			return nil, fmt.Errorf("%w: network_class %q does not exist", ErrValidation, spec.NetworkClass)
		}
		return nil, err
	}
	if !class.Spec.UsableBy(tenantID) {
		return nil, fmt.Errorf("%w: network_class %q is not available to tenant %q", ErrValidation, spec.NetworkClass, tenantID)
	}
	if err := validateNetworkVisibility(spec, class.Spec); err != nil {
		return nil, err
	}
	if spec.Visibility == "" {
		spec.Visibility = VisibilityPrivate
	}
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "CREATE", Resource: "Network", TenantID: tenantID, Name: name,
		Labels: md.Labels, Annotations: md.Annotations, Spec: admissionNetworkSpecJSON(spec),
	}); err != nil {
		return nil, err
	}
	// Always created Pending; Network-level values are allocated by
	// network-reconciler (watchPendingNetworks), same split as Subnet.
	out, err := s.networks.Create(ctx, tenantID, name, Network{
		Meta:   resource.ObjectMeta{Labels: md.Labels, Annotations: md.Annotations},
		Spec:   spec,
		Status: NetworkStatus{Phase: NetworkPhasePending},
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func validateNetworkVisibility(spec NetworkSpec, class NetworkClassSpec) error {
	switch spec.Visibility {
	case "", VisibilityPrivate:
	case VisibilityPublic:
		if !class.AllowPublicNetworks {
			return fmt.Errorf("%w: visibility=PUBLIC needs a NetworkClass with allow_public_networks (open, unvetted cross-tenant attach is only for address space meant to be shared, e.g. public IPs); share with specific tenants via shared_with_tenant_ids instead", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: visibility %q", ErrValidation, spec.Visibility)
	}
	return nil
}

// GetNetwork returns tenantID's own Network, or one shared with tenantID
// (or PUBLIC) -- a tenant can read a Network it may attach to. An empty
// tenantID (cross-tenant roles) reads any.
func (s *Service) GetNetwork(ctx context.Context, tenantID, id string) (*Network, error) {
	n, err := s.getNetworkAnyTenant(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if tenantID != "" && !n.UsableBy(tenantID) {
		return nil, ErrNetworkNotFound
	}
	return &n, nil
}

func (s *Service) ListNetworks(ctx context.Context, tenantID string) ([]Network, error) {
	return s.networks.List(ctx, tenantID)
}

func (s *Service) WatchNetworks(ctx context.Context, tenantID string, sinceRV int64) (<-chan resource.Event[Network], error) {
	return s.networks.Watch(ctx, tenantID, sinceRV, nil)
}

// getNetworkAnyTenant resolves a Network by id whoever owns it (it may be
// shared with the caller): ownerTenantID's namespace first, then a scan.
func (s *Service) getNetworkAnyTenant(ctx context.Context, ownerTenantID, id string) (Network, error) {
	if n, err := s.networks.Get(ctx, ownerTenantID, id); err == nil {
		return n, nil
	} else if !errors.Is(err, ErrNetworkNotFound) {
		return Network{}, err
	}
	all, err := s.networks.List(ctx, "")
	if err != nil {
		return Network{}, err
	}
	for _, n := range all {
		if n.Meta.ID == id {
			return n, nil
		}
	}
	return Network{}, ErrNetworkNotFound
}

func (s *Service) UpdateNetwork(ctx context.Context, network *Network) (*Network, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: network.Meta.Labels, Annotations: network.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	current, err := s.networks.Get(ctx, network.Meta.TenantID, network.Meta.ID)
	if err != nil {
		return nil, err
	}
	if network.Spec.NetworkClass != current.Spec.NetworkClass {
		return nil, fmt.Errorf("%w: spec.network_class cannot be changed", ErrValidation)
	}
	class, err := s.classes.Get(ctx, "", current.Spec.NetworkClass)
	if err != nil {
		return nil, err
	}
	if err := validateNetworkVisibility(network.Spec, class.Spec); err != nil {
		return nil, err
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, network.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	network.Meta.Finalizers = finalizers
	network.Status = current.Status // server-owned
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "UPDATE", Resource: "Network", TenantID: current.Meta.TenantID, Name: current.Meta.Name, ID: current.Meta.ID,
		Labels: network.Meta.Labels, Annotations: network.Meta.Annotations, Spec: admissionNetworkSpecJSON(network.Spec),
		OldObject: admissionNetworkObject(current),
	}); err != nil {
		return nil, err
	}
	out, err := s.networks.Update(ctx, *network)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteNetwork refuses while the Network still has Subnets. Its pool
// values return only once it's really gone (releaseNetwork), so a
// Finalizer holds them, same as Subnet.
func (s *Service) DeleteNetwork(ctx context.Context, tenantID, id string) error {
	current, err := s.networks.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	subnets, err := s.subnets.List(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, sn := range subnets {
		if sn.Spec.NetworkID == id {
			return fmt.Errorf("%w: Network %q still has Subnet %q", ErrInUse, current.Meta.Name, sn.Meta.Name)
		}
	}
	if r, err := s.reservationBlocking(ctx, id, ""); err != nil {
		return err
	} else if r != nil {
		return fmt.Errorf("%w: Network %q still has IP reservation %q (tenant %q)", ErrInUse, current.Meta.Name, r.Meta.ID, r.Meta.TenantID)
	}
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "DELETE", Resource: "Network", TenantID: tenantID, Name: current.Meta.Name, ID: id,
		OldObject: admissionNetworkObject(current),
	}); err != nil {
		return err
	}
	return s.networks.Delete(ctx, tenantID, id)
}

// SetNetworkStatusValues is the admin-only correction path for a
// Network's system-written values (the gRPC layer enforces admin).
func (s *Service) SetNetworkStatusValues(ctx context.Context, tenantID, id string, values map[string]int64, attributes map[string]string) (*Network, error) {
	for attempt := 0; attempt < 3; attempt++ {
		n, err := s.networks.Get(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
		n.Status.Values, n.Status.Attributes = values, attributes
		out, err := s.networks.Update(ctx, n)
		if errors.Is(err, ErrNetworkConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	return nil, ErrNetworkConflict
}

func (s *Service) watchPendingNetworks(ctx context.Context) {
	runWatchLoop(ctx, "networks", func(ctx context.Context, rv int64) (<-chan resource.Event[Network], error) {
		return s.WatchNetworks(ctx, "", rv)
	}, ErrNetworkHistoryPruned, s.remarkPools, func(e resource.Event[Network]) {
		if e.Type == EventDeleted {
			s.alloc.release(e.Object.Status.Allocations)
			s.deleteDefaultSecurityGroup(ctx, e.Object)
			return
		}
		if e.Type == EventAdded && e.Object.Status.Phase == NetworkPhasePending {
			n := e.Object
			s.tryAllocateNetwork(ctx, &n)
		}
	})
}

func (s *Service) retryPendingNetworks(ctx context.Context) {
	networks, err := s.networks.List(ctx, "")
	if err != nil {
		return
	}
	for i := range networks {
		switch {
		case networks[i].Status.Phase == NetworkPhasePending:
			s.tryAllocateNetwork(ctx, &networks[i])
		case networks[i].Status.DefaultSecurityGroupID == "" && networks[i].Meta.DeletedAt == nil:
			s.backfillDefaultSecurityGroup(ctx, networks[i]) // a Network from before default groups existed
		}
	}
}

// tryAllocateNetwork draws n's Network-level values (all or nothing) and
// marks it Ready, or leaves it Pending with an AllocationPending condition.
func (s *Service) tryAllocateNetwork(ctx context.Context, n *Network) {
	if n.Meta.DeletedAt != nil {
		return
	}
	class, err := s.classes.Get(ctx, "", n.Spec.NetworkClass)
	if err != nil {
		s.setNetworkPending(ctx, n, fmt.Sprintf("network_class %q: %v", n.Spec.NetworkClass, err))
		return
	}
	pools, err := s.poolsByID(ctx)
	if err != nil {
		return
	}
	sgID, err := s.ensureDefaultSecurityGroup(ctx, *n)
	if err != nil {
		s.setNetworkPending(ctx, n, fmt.Sprintf("default security group: %v", err))
		return
	}
	res, err := s.alloc.allocateAll(pools, class.Spec.Network, allocRequest{})
	if err != nil {
		s.setNetworkPending(ctx, n, err.Error())
		return
	}
	n.Status.DefaultSecurityGroupID = sgID
	n.Status.Phase = NetworkPhaseReady
	n.Status.Values, n.Status.Attributes, n.Status.Allocations = res.values, res.attributes, res.allocations
	n.Status.Conditions = upsertCondition(n.Status.Conditions, resource.Condition{Type: "AllocationPending", Status: resource.ConditionFalse, LastTransitionAt: time.Now()})
	updated, err := s.networks.Update(ctx, *n)
	if err != nil {
		s.alloc.release(res.allocations)
		return
	}
	*n = updated
}

func (s *Service) backfillDefaultSecurityGroup(ctx context.Context, n Network) {
	sgID, err := s.ensureDefaultSecurityGroup(ctx, n)
	if err != nil {
		slog.Warn("network: create default security group failed", "network_id", n.Meta.ID, "err", err)
		return
	}
	n.Status.DefaultSecurityGroupID = sgID
	if _, err := s.networks.Update(ctx, n); err != nil {
		slog.Warn("network: record default security group failed", "network_id", n.Meta.ID, "err", err)
	}
}

func (s *Service) setNetworkPending(ctx context.Context, n *Network, msg string) {
	n.Status.Conditions = upsertCondition(n.Status.Conditions, resource.Condition{
		Type: "AllocationPending", Status: resource.ConditionTrue, Message: msg, LastTransitionAt: time.Now(),
	})
	if updated, err := s.networks.Update(ctx, *n); err == nil {
		*n = updated
	}
}

// ---------------------------------------------------------------------------
// Subnet allocation

// validateSubnetRequest is Create-time validation: the Network exists, is
// the caller's own (only the owner adds Subnets -- a tenant it's shared
// with only attaches NICs) and isn't being deleted; the class can host a
// Subnet in this zone; requested_addresses matches what the class's CIDR
// source expects and doesn't overlap the Network's other Subnets.
func (s *Service) validateSubnetRequest(ctx context.Context, tenantID string, spec SubnetSpec) error {
	if spec.NetworkID == "" {
		return fmt.Errorf("%w: spec.network_id is required", ErrValidation)
	}
	if spec.Zone == "" {
		return fmt.Errorf("%w: spec.zone is required", ErrValidation)
	}
	network, err := s.networks.Get(ctx, tenantID, spec.NetworkID)
	if err != nil {
		if errors.Is(err, ErrNetworkNotFound) {
			return fmt.Errorf("%w: network_id %q does not exist in tenant %q", ErrValidation, spec.NetworkID, tenantID)
		}
		return err
	}
	if network.Meta.DeletedAt != nil {
		return fmt.Errorf("%w: network %q is being deleted", ErrValidation, spec.NetworkID)
	}
	class, err := s.classes.Get(ctx, "", network.Spec.NetworkClass)
	if err != nil {
		return err
	}
	pools, err := s.poolsByID(ctx)
	if err != nil {
		return err
	}
	sources, err := cidrSources(pools, class.Spec.SubnetRefs(spec.Zone))
	if err != nil {
		return err
	}
	if _, ok := sources[FamilyIPv4]; !ok {
		return fmt.Errorf("%w: network class %q gives zone %q no IPv4 address source, so no Subnet can be created there", ErrValidation, class.Meta.Name, spec.Zone)
	}
	requested := map[AddressFamily]SubnetAddress{}
	for _, a := range spec.RequestedAddresses {
		if err := validateAddressBlock(AddressBlock(a)); err != nil {
			return err
		}
		f := familyOf(a.CIDR)
		if _, dup := requested[f]; dup {
			return fmt.Errorf("%w: two requested %s addresses", ErrValidation, f)
		}
		requested[f] = a
	}
	for f, src := range sources {
		userMode := src.Spec.Kind() == PoolKindCidr && src.Spec.Cidr.Mode != CidrModeCarve
		if _, ok := requested[f]; ok != userMode {
			if userMode {
				return fmt.Errorf("%w: network class %q expects the %s CIDR in spec.requested_addresses", ErrValidation, class.Meta.Name, f)
			}
			return fmt.Errorf("%w: network class %q allocates the %s CIDR itself; leave it out of spec.requested_addresses", ErrValidation, class.Meta.Name, f)
		}
	}
	for f := range requested {
		if _, ok := sources[f]; !ok {
			return fmt.Errorf("%w: network class %q has no %s address source for zone %q", ErrValidation, class.Meta.Name, f, spec.Zone)
		}
	}
	if v4, ok := requested[FamilyIPv4]; ok {
		if err := validateAllocatableIPRanges(v4.CIDR, spec.AllocatableIPRanges); err != nil {
			return fmt.Errorf("%w: spec.allocatable_ip_ranges: %v", ErrValidation, err)
		}
	}
	avoid, err := s.networkCIDRs(ctx, tenantID, spec.NetworkID, "")
	if err != nil {
		return err
	}
	for _, a := range requested {
		_, n, _ := net.ParseCIDR(a.CIDR)
		if overlapsNets(n, avoid) {
			return fmt.Errorf("%w: requested CIDR %s overlaps another Subnet of network %q", ErrValidation, a.CIDR, spec.NetworkID)
		}
	}
	return validateIPs("spec.dns_servers", spec.DNSServers)
}

func validateIPs(field string, ips []string) error {
	for _, ip := range ips {
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("%w: %s: %q is not an IP address", ErrValidation, field, ip)
		}
	}
	return nil
}

// networkCIDRs is every address already used, or requested, by the
// Network's Subnets other than excludeSubnetID: within one Network (one
// routing domain) CIDRs must not overlap.
func (s *Service) networkCIDRs(ctx context.Context, ownerTenantID, networkID, excludeSubnetID string) ([]*net.IPNet, error) {
	subnets, err := s.subnets.List(ctx, ownerTenantID)
	if err != nil {
		return nil, err
	}
	var out []*net.IPNet
	for _, sn := range subnets {
		if sn.Spec.NetworkID != networkID || sn.Meta.ID == excludeSubnetID {
			continue
		}
		for _, a := range append(slices.Clone(sn.Status.Addresses), sn.Spec.RequestedAddresses...) {
			if _, n, err := net.ParseCIDR(a.CIDR); err == nil {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// tryAllocateSubnet draws sn's Subnet-level values from its Network's
// class for sn's zone (all or nothing) once the Network itself is Ready,
// and marks sn Ready -- or leaves it Pending with an AllocationPending
// condition, retried by the sweep.
func (s *Service) tryAllocateSubnet(ctx context.Context, sn *Subnet) {
	if sn.Meta.DeletedAt != nil {
		return // being deleted (held by a Finalizer): never allocate now
	}
	network, err := s.networks.Get(ctx, sn.Meta.TenantID, sn.Spec.NetworkID)
	if err != nil {
		s.setSubnetPending(ctx, sn, fmt.Sprintf("network %q: %v", sn.Spec.NetworkID, err))
		return
	}
	if network.Status.Phase != NetworkPhaseReady {
		s.setSubnetPending(ctx, sn, fmt.Sprintf("network %q is not Ready yet", network.Meta.ID))
		return
	}
	class, err := s.classes.Get(ctx, "", network.Spec.NetworkClass)
	if err != nil {
		s.setSubnetPending(ctx, sn, fmt.Sprintf("network_class %q: %v", network.Spec.NetworkClass, err))
		return
	}
	pools, err := s.poolsByID(ctx)
	if err != nil {
		return
	}
	avoid, err := s.networkCIDRs(ctx, sn.Meta.TenantID, sn.Spec.NetworkID, sn.Meta.ID)
	if err != nil {
		return
	}
	res, err := s.alloc.allocateAll(pools, class.Spec.SubnetRefs(sn.Spec.Zone), allocRequest{
		requested: sn.Spec.RequestedAddresses,
		avoid:     avoid,
		placement: class.Spec.GatewayPlacement,
	})
	if err != nil {
		s.setSubnetPending(ctx, sn, err.Error())
		return
	}
	sn.Status.Phase = SubnetPhaseReady
	sn.Status.Addresses, sn.Status.Values, sn.Status.Attributes, sn.Status.Allocations = res.addresses, res.values, res.attributes, res.allocations
	sn.Status.Conditions = upsertCondition(sn.Status.Conditions, resource.Condition{Type: "AllocationPending", Status: resource.ConditionFalse, LastTransitionAt: time.Now()})
	updated, err := s.subnets.Update(ctx, *sn)
	if err != nil {
		s.alloc.release(res.allocations)
		return
	}
	*sn = updated
}

func (s *Service) setSubnetPending(ctx context.Context, sn *Subnet, msg string) {
	for _, c := range sn.Status.Conditions {
		if c.Type == "AllocationPending" && c.Status == resource.ConditionTrue && c.Message == msg {
			return // already says so: don't churn a Modified event every sweep
		}
	}
	sn.Status.Conditions = upsertCondition(sn.Status.Conditions, resource.Condition{
		Type: "AllocationPending", Status: resource.ConditionTrue, Message: msg, LastTransitionAt: time.Now(),
	})
	if updated, err := s.subnets.Update(ctx, *sn); err == nil {
		*sn = updated
	}
}

// SetSubnetStatusValues is the admin-only correction path for a Subnet's
// system-written values (the gRPC layer enforces admin).
func (s *Service) SetSubnetStatusValues(ctx context.Context, tenantID, id string, values map[string]int64, attributes map[string]string) (*Subnet, error) {
	for attempt := 0; attempt < 3; attempt++ {
		sn, err := s.subnets.Get(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
		sn.Status.Values, sn.Status.Attributes = values, attributes
		out, err := s.subnets.Update(ctx, sn)
		if errors.Is(err, ErrSubnetConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	return nil, ErrSubnetConflict
}

// attachContext is subnet's Network/NetworkClass context for VNAP/SNAP.
func (s *Service) attachContext(ctx context.Context, subnet Subnet) AttachContext {
	out := AttachContext{NetworkID: subnet.Spec.NetworkID, SubnetValues: subnet.Status.Values, SubnetAttributes: subnet.Status.Attributes}
	network, err := s.getNetworkAnyTenant(ctx, subnet.Meta.TenantID, subnet.Spec.NetworkID)
	if err != nil {
		return out
	}
	out.NetworkLabels, out.NetworkValues, out.NetworkAttributes = network.Meta.Labels, network.Status.Values, network.Status.Attributes
	if class, err := s.classes.Get(ctx, "", network.Spec.NetworkClass); err == nil {
		out.NetworkClass, out.NetworkClassAttributes, out.MTU = class.Meta.Name, maps.Clone(class.Spec.Attributes), class.Spec.MTU
	}
	return out
}
