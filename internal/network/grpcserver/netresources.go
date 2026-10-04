package grpcserver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/resource"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// ---------------------------------------------------------------------------
// AllocationPoolService

type AllocationPoolServer struct {
	networkv1.UnimplementedAllocationPoolServiceServer
	svc *network.Service
}

func NewAllocationPoolServer(svc *network.Service) *AllocationPoolServer {
	return &AllocationPoolServer{svc: svc}
}

func (s *AllocationPoolServer) Create(ctx context.Context, req *networkv1.CreateAllocationPoolRequest) (*networkv1.AllocationPool, error) {
	p, err := s.svc.CreateAllocationPool(ctx, req.GetName(), fromPoolSpec(req.GetSpec()), resource.Metadata{Labels: req.GetLabels(), Annotations: req.GetAnnotations()})
	if err != nil {
		return nil, toStatus(err)
	}
	return toPool(*p), nil
}

func (s *AllocationPoolServer) Get(ctx context.Context, req *networkv1.GetAllocationPoolRequest) (*networkv1.AllocationPool, error) {
	p, err := s.svc.GetAllocationPool(ctx, req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toPool(*p), nil
}

func (s *AllocationPoolServer) List(ctx context.Context, _ *networkv1.ListAllocationPoolsRequest) (*networkv1.ListAllocationPoolsResponse, error) {
	pools, err := s.svc.ListAllocationPools(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListAllocationPoolsResponse{}
	for _, p := range pools {
		out.Items = append(out.Items, toPool(p))
	}
	return out, nil
}

func (s *AllocationPoolServer) Update(ctx context.Context, req *networkv1.UpdateAllocationPoolRequest) (*networkv1.AllocationPool, error) {
	p := fromPool(req.GetPool())
	out, err := s.svc.UpdateAllocationPool(ctx, &p)
	if err != nil {
		return nil, toStatus(err)
	}
	return toPool(*out), nil
}

func (s *AllocationPoolServer) Delete(ctx context.Context, req *networkv1.DeleteAllocationPoolRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteAllocationPool(ctx, req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *AllocationPoolServer) Watch(req *networkv1.WatchAllocationPoolsRequest, stream networkv1.AllocationPoolService_WatchServer) error {
	events, err := s.svc.WatchAllocationPools(stream.Context(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		out := &networkv1.AllocationPoolEvent{Type: networkv1.AllocationPoolEvent_Type(eventType(e.Type)), ResourceVersion: e.ResourceVersion}
		if e.Type != network.EventBookmark {
			out.Pool = toPool(e.Object)
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

// eventType maps a store event to the shared numbering every *Event.Type
// enum in network.proto uses (ADDED=1 ... BOOKMARK=4).
func eventType(t resource.EventType) int32 {
	switch t {
	case network.EventAdded:
		return 1
	case network.EventModified:
		return 2
	case network.EventDeleted:
		return 3
	default:
		return 4
	}
}

func fromPoolSpec(s *networkv1.AllocationPoolSpec) network.AllocationPoolSpec {
	out := network.AllocationPoolSpec{Attributes: s.GetAttributes()}
	switch k := s.GetKind().(type) {
	case *networkv1.AllocationPoolSpec_Entries:
		out.Entries = []network.PoolEntry{}
		for _, e := range k.Entries.GetEntries() {
			pe := network.PoolEntry{Key: e.GetKey(), Values: e.GetValues(), Attributes: e.GetAttributes()}
			for _, a := range e.GetAddresses() {
				pe.Addresses = append(pe.Addresses, network.AddressBlock{CIDR: a.GetCidr(), GatewayIP: a.GetGatewayIp()})
			}
			out.Entries = append(out.Entries, pe)
		}
	case *networkv1.AllocationPoolSpec_Integer:
		out.Integer = []network.IntRange{}
		for _, r := range k.Integer.GetRanges() {
			out.Integer = append(out.Integer, network.IntRange{Lo: r.GetLo(), Hi: r.GetHi()})
		}
	case *networkv1.AllocationPoolSpec_Cidr:
		out.Cidr = &network.CidrPoolSpec{
			Family: fromFamily(k.Cidr.GetFamily()), Mode: fromCidrMode(k.Cidr.GetMode()),
			Blocks: k.Cidr.GetBlocks(), PrefixLength: k.Cidr.GetPrefixLength(),
		}
	}
	return out
}

func toPoolSpec(s network.AllocationPoolSpec) *networkv1.AllocationPoolSpec {
	out := &networkv1.AllocationPoolSpec{Attributes: s.Attributes}
	switch s.Kind() {
	case network.PoolKindEntries:
		ep := &networkv1.EntriesPool{}
		for _, e := range s.Entries {
			pe := &networkv1.PoolEntry{Key: e.Key, Values: e.Values, Attributes: e.Attributes}
			for _, a := range e.Addresses {
				pe.Addresses = append(pe.Addresses, &networkv1.AddressBlock{Cidr: a.CIDR, GatewayIp: a.GatewayIP})
			}
			ep.Entries = append(ep.Entries, pe)
		}
		out.Kind = &networkv1.AllocationPoolSpec_Entries{Entries: ep}
	case network.PoolKindInteger:
		ip := &networkv1.IntegerPool{}
		for _, r := range s.Integer {
			ip.Ranges = append(ip.Ranges, &networkv1.IntRange{Lo: r.Lo, Hi: r.Hi})
		}
		out.Kind = &networkv1.AllocationPoolSpec_Integer{Integer: ip}
	case network.PoolKindCidr:
		out.Kind = &networkv1.AllocationPoolSpec_Cidr{Cidr: &networkv1.CidrPool{
			Family: toFamily(s.Cidr.Family), Mode: toCidrMode(s.Cidr.Mode), Blocks: s.Cidr.Blocks, PrefixLength: s.Cidr.PrefixLength,
		}}
	}
	return out
}

func fromFamily(f networkv1.AddressFamily) network.AddressFamily {
	if f == networkv1.AddressFamily_IPV6 {
		return network.FamilyIPv6
	}
	return network.FamilyIPv4
}

func toFamily(f network.AddressFamily) networkv1.AddressFamily {
	if f == network.FamilyIPv6 {
		return networkv1.AddressFamily_IPV6
	}
	return networkv1.AddressFamily_IPV4
}

func fromCidrMode(m networkv1.CidrMode) network.CidrMode {
	switch m {
	case networkv1.CidrMode_USER_ANY:
		return network.CidrModeUserAny
	case networkv1.CidrMode_USER_WITHIN_BLOCKS:
		return network.CidrModeUserWithinBlocks
	case networkv1.CidrMode_CARVE:
		return network.CidrModeCarve
	}
	return ""
}

func toCidrMode(m network.CidrMode) networkv1.CidrMode {
	switch m {
	case network.CidrModeUserAny:
		return networkv1.CidrMode_USER_ANY
	case network.CidrModeUserWithinBlocks:
		return networkv1.CidrMode_USER_WITHIN_BLOCKS
	case network.CidrModeCarve:
		return networkv1.CidrMode_CARVE
	}
	return networkv1.CidrMode_CIDR_MODE_UNSPECIFIED
}

func toPool(p network.AllocationPool) *networkv1.AllocationPool {
	return &networkv1.AllocationPool{Meta: toMetaProto(p.Meta), Spec: toPoolSpec(p.Spec), Status: &networkv1.AllocationPoolStatus{Allocated: p.Status.Allocated}}
}

func fromPool(p *networkv1.AllocationPool) network.AllocationPool {
	return network.AllocationPool{Meta: fromMetaProto(p.GetMeta()), Spec: fromPoolSpec(p.GetSpec())}
}

func toAllocationsProto(as []network.Allocation) []*networkv1.Allocation {
	var out []*networkv1.Allocation
	for _, a := range as {
		out = append(out, &networkv1.Allocation{PoolId: a.PoolID, Name: a.Name, Integer: a.Integer, EntryKey: a.EntryKey, Cidr: a.CIDR})
	}
	return out
}

func fromAllocationsProto(as []*networkv1.Allocation) []network.Allocation {
	var out []network.Allocation
	for _, a := range as {
		out = append(out, network.Allocation{PoolID: a.GetPoolId(), Name: a.GetName(), Integer: a.GetInteger(), EntryKey: a.GetEntryKey(), CIDR: a.GetCidr()})
	}
	return out
}

// ---------------------------------------------------------------------------
// NetworkClassService

type NetworkClassServer struct {
	networkv1.UnimplementedNetworkClassServiceServer
	svc *network.Service
}

func NewNetworkClassServer(svc *network.Service) *NetworkClassServer {
	return &NetworkClassServer{svc: svc}
}

func (s *NetworkClassServer) Create(ctx context.Context, req *networkv1.CreateNetworkClassRequest) (*networkv1.NetworkClass, error) {
	c, err := s.svc.CreateNetworkClass(ctx, req.GetName(), fromClassSpec(req.GetSpec()), resource.Metadata{Labels: req.GetLabels(), Annotations: req.GetAnnotations()})
	if err != nil {
		return nil, toStatus(err)
	}
	return toClass(*c), nil
}

func (s *NetworkClassServer) Get(ctx context.Context, req *networkv1.GetNetworkClassRequest) (*networkv1.NetworkClass, error) {
	c, err := s.svc.GetNetworkClass(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toClass(*c), nil
}

func (s *NetworkClassServer) List(ctx context.Context, req *networkv1.ListNetworkClassesRequest) (*networkv1.ListNetworkClassesResponse, error) {
	classes, err := s.svc.ListNetworkClasses(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListNetworkClassesResponse{}
	for _, c := range classes {
		out.Items = append(out.Items, toClass(c))
	}
	return out, nil
}

func (s *NetworkClassServer) Update(ctx context.Context, req *networkv1.UpdateNetworkClassRequest) (*networkv1.NetworkClass, error) {
	c := fromClass(req.GetNetworkClass())
	out, err := s.svc.UpdateNetworkClass(ctx, &c)
	if err != nil {
		return nil, toStatus(err)
	}
	return toClass(*out), nil
}

func (s *NetworkClassServer) Delete(ctx context.Context, req *networkv1.DeleteNetworkClassRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteNetworkClass(ctx, req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *NetworkClassServer) Watch(req *networkv1.WatchNetworkClassesRequest, stream networkv1.NetworkClassService_WatchServer) error {
	events, err := s.svc.WatchNetworkClasses(stream.Context(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		out := &networkv1.NetworkClassEvent{Type: networkv1.NetworkClassEvent_Type(eventType(e.Type)), ResourceVersion: e.ResourceVersion}
		if e.Type != network.EventBookmark {
			out.NetworkClass = toClass(e.Object)
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func fromRefs(refs []*networkv1.PoolRef) []network.PoolRef {
	var out []network.PoolRef
	for _, r := range refs {
		out = append(out, network.PoolRef{PoolID: r.GetPoolId(), Name: r.GetName()})
	}
	return out
}

func toRefs(refs []network.PoolRef) []*networkv1.PoolRef {
	var out []*networkv1.PoolRef
	for _, r := range refs {
		out = append(out, &networkv1.PoolRef{PoolId: r.PoolID, Name: r.Name})
	}
	return out
}

func fromClassSpec(c *networkv1.NetworkClassSpec) network.NetworkClassSpec {
	out := network.NetworkClassSpec{
		Network: fromRefs(c.GetNetwork()), Subnet: map[string][]network.PoolRef{}, Attributes: c.GetAttributes(),
		Visibility: fromVisibility(c.GetVisibility()), SharedWithTenantIDs: c.GetSharedWithTenantIds(),
		AllowPublicNetworks: c.GetAllowPublicNetworks(), DefaultDNSServers: map[string][]string{}, MTU: c.GetMtu(),
		GatewayPlacement: fromPlacement(c.GetGatewayPlacement()), HostAggregateSelector: c.GetHostAggregateSelector(),
	}
	for z, l := range c.GetSubnet() {
		out.Subnet[z] = fromRefs(l.GetRefs())
	}
	for z, d := range c.GetDefaultDnsServers() {
		out.DefaultDNSServers[z] = d.GetServers()
	}
	return out
}

func toClassSpec(c network.NetworkClassSpec) *networkv1.NetworkClassSpec {
	out := &networkv1.NetworkClassSpec{
		Network: toRefs(c.Network), Subnet: map[string]*networkv1.PoolRefList{}, Attributes: c.Attributes,
		Visibility: toVisibility(c.Visibility), SharedWithTenantIds: c.SharedWithTenantIDs,
		AllowPublicNetworks: c.AllowPublicNetworks, DefaultDnsServers: map[string]*networkv1.ZoneDNS{}, Mtu: c.MTU,
		GatewayPlacement: toPlacement(c.GatewayPlacement), HostAggregateSelector: c.HostAggregateSelector,
	}
	for z, refs := range c.Subnet {
		out.Subnet[z] = &networkv1.PoolRefList{Refs: toRefs(refs)}
	}
	for z, servers := range c.DefaultDNSServers {
		out.DefaultDnsServers[z] = &networkv1.ZoneDNS{Servers: servers}
	}
	return out
}

func fromPlacement(p networkv1.GatewayPlacement) network.GatewayPlacement {
	if p == networkv1.GatewayPlacement_LAST {
		return network.GatewayLast
	}
	return network.GatewayFirst
}

func toPlacement(p network.GatewayPlacement) networkv1.GatewayPlacement {
	if p == network.GatewayLast {
		return networkv1.GatewayPlacement_LAST
	}
	return networkv1.GatewayPlacement_FIRST
}

func fromVisibility(v networkv1.Visibility) network.Visibility {
	switch v {
	case networkv1.Visibility_VISIBILITY_PUBLIC:
		return network.VisibilityPublic
	case networkv1.Visibility_VISIBILITY_PRIVATE:
		return network.VisibilityPrivate
	}
	return network.VisibilityUnspecified
}

func toVisibility(v network.Visibility) networkv1.Visibility {
	switch v {
	case network.VisibilityPublic:
		return networkv1.Visibility_VISIBILITY_PUBLIC
	case network.VisibilityPrivate:
		return networkv1.Visibility_VISIBILITY_PRIVATE
	}
	return networkv1.Visibility_VISIBILITY_UNSPECIFIED
}

func toClass(c network.NetworkClass) *networkv1.NetworkClass {
	return &networkv1.NetworkClass{Meta: toMetaProto(c.Meta), Spec: toClassSpec(c.Spec)}
}

func fromClass(c *networkv1.NetworkClass) network.NetworkClass {
	return network.NetworkClass{Meta: fromMetaProto(c.GetMeta()), Spec: fromClassSpec(c.GetSpec())}
}

// ---------------------------------------------------------------------------
// NetworkService

type NetworkServer struct {
	networkv1.UnimplementedNetworkServiceServer
	svc *network.Service
}

func NewNetworkServer(svc *network.Service) *NetworkServer {
	return &NetworkServer{svc: svc}
}

func (s *NetworkServer) Create(ctx context.Context, req *networkv1.CreateNetworkRequest) (*networkv1.Network, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	n, err := s.svc.CreateNetwork(ctx, req.GetTenantId(), req.GetName(), fromNetworkSpec(req.GetSpec()), resource.Metadata{Labels: req.GetLabels(), Annotations: req.GetAnnotations()})
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetwork(*n), nil
}

func (s *NetworkServer) Get(ctx context.Context, req *networkv1.GetNetworkRequest) (*networkv1.Network, error) {
	n, err := s.svc.GetNetwork(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetwork(*n), nil
}

func (s *NetworkServer) List(ctx context.Context, req *networkv1.ListNetworksRequest) (*networkv1.ListNetworksResponse, error) {
	networks, err := s.svc.ListNetworks(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListNetworksResponse{}
	for _, n := range networks {
		out.Items = append(out.Items, toNetwork(n))
	}
	return out, nil
}

func (s *NetworkServer) Update(ctx context.Context, req *networkv1.UpdateNetworkRequest) (*networkv1.Network, error) {
	if req.GetTenantId() == "" || req.GetTenantId() != req.GetNetwork().GetMeta().GetTenantId() {
		return nil, status.Error(codes.InvalidArgument, "tenant_id must be set and match network.meta.tenant_id")
	}
	n := fromNetwork(req.GetNetwork())
	out, err := s.svc.UpdateNetwork(ctx, &n)
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetwork(*out), nil
}

func (s *NetworkServer) Delete(ctx context.Context, req *networkv1.DeleteNetworkRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteNetwork(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *NetworkServer) Watch(req *networkv1.WatchNetworksRequest, stream networkv1.NetworkService_WatchServer) error {
	events, err := s.svc.WatchNetworks(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		out := &networkv1.NetworkEvent{Type: networkv1.NetworkEvent_Type(eventType(e.Type)), ResourceVersion: e.ResourceVersion}
		if e.Type != network.EventBookmark {
			out.Network = toNetwork(e.Object)
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

// SetStatusValues: see SubnetServer.SetStatusValues for why the admin
// check lives here.
func (s *NetworkServer) SetStatusValues(ctx context.Context, req *networkv1.SetNetworkStatusValuesRequest) (*networkv1.Network, error) {
	if !authn.CallerIsAdminFromContext(ctx) {
		return nil, status.Error(codes.PermissionDenied, "only an admin may correct system-written values")
	}
	n, err := s.svc.SetNetworkStatusValues(ctx, req.GetTenantId(), req.GetId(), req.GetValues(), req.GetAttributes())
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetwork(*n), nil
}

func fromNetworkSpec(s *networkv1.NetworkSpec) network.NetworkSpec {
	return network.NetworkSpec{NetworkClass: s.GetNetworkClass(), DNSSuffix: s.GetDnsSuffix(), Visibility: fromVisibility(s.GetVisibility()), SharedWithTenantIDs: s.GetSharedWithTenantIds()}
}

func toNetwork(n network.Network) *networkv1.Network {
	st := &networkv1.NetworkStatus{Phase: string(n.Status.Phase), Values: n.Status.Values, Attributes: n.Status.Attributes, Allocations: toAllocationsProto(n.Status.Allocations)}
	for _, c := range n.Status.Conditions {
		st.Conditions = append(st.Conditions, toConditionProto(c))
	}
	return &networkv1.Network{
		Meta:   toMetaProto(n.Meta),
		Spec:   &networkv1.NetworkSpec{NetworkClass: n.Spec.NetworkClass, DnsSuffix: n.Spec.DNSSuffix, Visibility: toVisibility(n.Spec.Visibility), SharedWithTenantIds: n.Spec.SharedWithTenantIDs},
		Status: st,
	}
}

// fromNetwork leaves status empty: it's server-owned, and UpdateNetwork
// keeps the stored one regardless.
func fromNetwork(n *networkv1.Network) network.Network {
	return network.Network{Meta: fromMetaProto(n.GetMeta()), Spec: fromNetworkSpec(n.GetSpec())}
}
