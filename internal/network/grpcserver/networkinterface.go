package grpcserver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/resource"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

type NetworkInterfaceServer struct {
	networkv1.UnimplementedNetworkInterfaceServiceServer
	svc *network.Service
}

func NewNetworkInterfaceServer(svc *network.Service) *NetworkInterfaceServer {
	return &NetworkInterfaceServer{svc: svc}
}

func (s *NetworkInterfaceServer) Create(ctx context.Context, req *networkv1.CreateNetworkInterfaceRequest) (*networkv1.NetworkInterface, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	n, err := s.svc.CreateNetworkInterfaceWithMetadata(ctx, req.GetTenantId(), req.GetName(), fromNetworkInterfaceSpec(req.GetSpec()),
		resource.Metadata{Labels: req.GetLabels(), Annotations: req.GetAnnotations()})
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetworkInterface(*n), nil
}

func (s *NetworkInterfaceServer) Get(ctx context.Context, req *networkv1.GetNetworkInterfaceRequest) (*networkv1.NetworkInterface, error) {
	n, err := s.svc.GetNetworkInterface(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetworkInterface(*n), nil
}

func (s *NetworkInterfaceServer) List(ctx context.Context, req *networkv1.ListNetworkInterfacesRequest) (*networkv1.ListNetworkInterfacesResponse, error) {
	ifaces, err := s.svc.ListNetworkInterfaces(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListNetworkInterfacesResponse{}
	for _, n := range ifaces {
		out.Items = append(out.Items, toNetworkInterface(n))
	}
	return out, nil
}

func (s *NetworkInterfaceServer) Update(ctx context.Context, req *networkv1.UpdateNetworkInterfaceRequest) (*networkv1.NetworkInterface, error) {
	if req.GetTenantId() == "" || req.GetTenantId() != req.GetNetworkInterface().GetMeta().GetTenantId() {
		return nil, status.Error(codes.InvalidArgument, "tenant_id must be set and match network_interface.meta.tenant_id")
	}
	n := fromNetworkInterface(req.GetNetworkInterface())
	updated, err := s.svc.UpdateNetworkInterface(ctx, &n)
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetworkInterface(*updated), nil
}

func (s *NetworkInterfaceServer) SetSecurityGroups(ctx context.Context, req *networkv1.SetSecurityGroupsRequest) (*networkv1.NetworkInterface, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	n, err := s.svc.SetSecurityGroups(ctx, req.GetTenantId(), req.GetId(), req.GetSecurityGroupIds())
	if err != nil {
		return nil, toStatus(err)
	}
	return toNetworkInterface(*n), nil
}

// GetSecurityPolicy is compute's boot-time read of what the host must
// enforce (not served through api-gateway).
func (s *NetworkInterfaceServer) GetSecurityPolicy(ctx context.Context, req *networkv1.GetSecurityPolicyRequest) (*networkv1.SecurityPolicy, error) {
	n, err := s.svc.GetNetworkInterface(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	p, err := s.svc.SecurityPolicy(ctx, *n)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.SecurityPolicy{SecurityGroupIds: p.SecurityGroupIDs}
	conv := func(rs []network.PolicyRule) []*networkv1.SecurityPolicyRule {
		var o []*networkv1.SecurityPolicyRule
		for _, r := range rs {
			o = append(o, &networkv1.SecurityPolicyRule{Protocol: r.Protocol, PortRange: r.PortRange, Cidr: r.CIDR, Set: r.Set})
		}
		return o
	}
	out.IngressRules, out.EgressRules = conv(p.IngressRules), conv(p.EgressRules)
	for _, a := range p.Sets {
		out.Sets = append(out.Sets, &networkv1.AddressSet{Name: a.Name, Version: a.Version, Members: a.Members})
	}
	return out, nil
}

func (s *NetworkInterfaceServer) Delete(ctx context.Context, req *networkv1.DeleteNetworkInterfaceRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteNetworkInterface(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *NetworkInterfaceServer) Watch(req *networkv1.WatchNetworkInterfacesRequest, stream networkv1.NetworkInterfaceService_WatchServer) error {
	events, err := s.svc.WatchNetworkInterfaces(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		if err := stream.Send(toNetworkInterfaceEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func fromNetworkInterfaceSpec(s *networkv1.NetworkInterfaceSpec) network.NetworkInterfaceSpec {
	spec := network.NetworkInterfaceSpec{
		VMID:      s.GetVmId(),
		SubnetID:  s.GetSubnetId(),
		NetworkID: s.GetNetworkId(),
		Zone:      s.GetZone(),

		SecurityGroupIDs: s.GetSecurityGroupIds(),
	}
	return spec
}

func toNetworkInterfaceSpec(s network.NetworkInterfaceSpec) *networkv1.NetworkInterfaceSpec {
	out := &networkv1.NetworkInterfaceSpec{
		VmId:      s.VMID,
		SubnetId:  s.SubnetID,
		NetworkId: s.NetworkID,
		Zone:      s.Zone,

		SecurityGroupIds: s.SecurityGroupIDs,
	}
	return out
}

func toNetworkInterfaceStatusProto(st network.NetworkInterfaceStatus) *networkv1.NetworkInterfaceStatus {
	out := &networkv1.NetworkInterfaceStatus{
		Phase:      string(st.Phase),
		IpAddress:  st.IPAddress,
		MacAddress: st.MACAddress,
		Hypervisor: st.Hypervisor,
		SubnetId:   st.SubnetID,
	}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, toConditionProto(c))
	}
	return out
}

func fromNetworkInterfaceStatusProto(st *networkv1.NetworkInterfaceStatus) network.NetworkInterfaceStatus {
	out := network.NetworkInterfaceStatus{
		Phase:      network.NetworkInterfacePhase(st.GetPhase()),
		IPAddress:  st.GetIpAddress(),
		MACAddress: st.GetMacAddress(),
		Hypervisor: st.GetHypervisor(),
		SubnetID:   st.GetSubnetId(),
	}
	for _, c := range st.GetConditions() {
		out.Conditions = append(out.Conditions, fromConditionProto(c))
	}
	return out
}

func toNetworkInterface(n network.NetworkInterface) *networkv1.NetworkInterface {
	return &networkv1.NetworkInterface{
		Meta:   toMetaProto(n.Meta),
		Spec:   toNetworkInterfaceSpec(n.Spec),
		Status: toNetworkInterfaceStatusProto(n.Status),
	}
}

func fromNetworkInterface(n *networkv1.NetworkInterface) network.NetworkInterface {
	return network.NetworkInterface{
		Meta:   fromMetaProto(n.GetMeta()),
		Spec:   fromNetworkInterfaceSpec(n.GetSpec()),
		Status: fromNetworkInterfaceStatusProto(n.GetStatus()),
	}
}

func toNetworkInterfaceEvent(e network.NetworkInterfaceEvent) *networkv1.NetworkInterfaceEvent {
	out := &networkv1.NetworkInterfaceEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case network.EventAdded:
		out.Type = networkv1.NetworkInterfaceEvent_ADDED
	case network.EventModified:
		out.Type = networkv1.NetworkInterfaceEvent_MODIFIED
	case network.EventDeleted:
		out.Type = networkv1.NetworkInterfaceEvent_DELETED
	case network.EventBookmark:
		out.Type = networkv1.NetworkInterfaceEvent_BOOKMARK
	}
	if out.Type != networkv1.NetworkInterfaceEvent_BOOKMARK {
		out.NetworkInterface = toNetworkInterface(e.Object)
	}
	return out
}
