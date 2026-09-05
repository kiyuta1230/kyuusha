// Package grpcserver adapts internal/network.Service to the generated
// SubnetServiceServer/NetworkInterfaceServiceServer interfaces. It only
// translates between wire types and domain types; all behavior lives in
// network.Service.
package grpcserver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"gitlab.com/ki.yuta1230/kyuusha/internal/network"
	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"

	networkv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/network/v1"
	resourcev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

type SubnetServer struct {
	networkv1.UnimplementedSubnetServiceServer
	svc *network.Service
}

func NewSubnetServer(svc *network.Service) *SubnetServer {
	return &SubnetServer{svc: svc}
}

func (s *SubnetServer) Create(ctx context.Context, req *networkv1.CreateSubnetRequest) (*networkv1.Subnet, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	sn, err := s.svc.CreateSubnet(ctx, req.GetTenantId(), req.GetName(), fromSubnetSpec(req.GetSpec()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toSubnet(*sn), nil
}

func (s *SubnetServer) Get(ctx context.Context, req *networkv1.GetSubnetRequest) (*networkv1.Subnet, error) {
	sn, err := s.svc.GetSubnet(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toSubnet(*sn), nil
}

func (s *SubnetServer) List(ctx context.Context, req *networkv1.ListSubnetsRequest) (*networkv1.ListSubnetsResponse, error) {
	subnets, err := s.svc.ListSubnets(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListSubnetsResponse{}
	for _, sn := range subnets {
		out.Items = append(out.Items, toSubnet(sn))
	}
	return out, nil
}

func (s *SubnetServer) Update(ctx context.Context, req *networkv1.UpdateSubnetRequest) (*networkv1.Subnet, error) {
	if req.GetTenantId() == "" || req.GetTenantId() != req.GetSubnet().GetMeta().GetTenantId() {
		return nil, status.Error(codes.InvalidArgument, "tenant_id must be set and match subnet.meta.tenant_id")
	}
	sn := fromSubnet(req.GetSubnet())
	updated, err := s.svc.UpdateSubnet(ctx, &sn)
	if err != nil {
		return nil, toStatus(err)
	}
	return toSubnet(*updated), nil
}

func (s *SubnetServer) Delete(ctx context.Context, req *networkv1.DeleteSubnetRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteSubnet(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *SubnetServer) Watch(req *networkv1.WatchSubnetsRequest, stream networkv1.SubnetService_WatchServer) error {
	events, err := s.svc.WatchSubnets(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		if err := stream.Send(toSubnetEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, network.ErrSubnetNotFound), errors.Is(err, network.ErrNetworkInterfaceNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, network.ErrSubnetConflict), errors.Is(err, network.ErrNetworkInterfaceConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, network.ErrSubnetHistoryPruned), errors.Is(err, network.ErrNetworkInterfaceHistoryPruned):
		return status.Error(codes.OutOfRange, err.Error())
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func fromSubnetSpec(s *networkv1.SubnetSpec) network.SubnetSpec {
	return network.SubnetSpec{
		Zone:                s.GetZone(),
		CIDR:                s.GetCidr(),
		GatewayIP:           s.GetGatewayIp(),
		DNSServers:          s.GetDnsServers(),
		SharedWithTenantIDs: s.GetSharedWithTenantIds(),
		DNSSuffix:           s.GetDnsSuffix(),
	}
}

func toSubnetSpec(s network.SubnetSpec) *networkv1.SubnetSpec {
	return &networkv1.SubnetSpec{
		Zone:                s.Zone,
		Cidr:                s.CIDR,
		GatewayIp:           s.GatewayIP,
		DnsServers:          s.DNSServers,
		SharedWithTenantIds: s.SharedWithTenantIDs,
		DnsSuffix:           s.DNSSuffix,
	}
}

func toSubnetStatusProto(st network.SubnetStatus) *networkv1.SubnetStatus {
	out := &networkv1.SubnetStatus{Phase: string(st.Phase), VlanId: st.VLANID}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, toConditionProto(c))
	}
	return out
}

func fromSubnetStatusProto(st *networkv1.SubnetStatus) network.SubnetStatus {
	out := network.SubnetStatus{Phase: network.SubnetPhase(st.GetPhase()), VLANID: st.GetVlanId()}
	for _, c := range st.GetConditions() {
		out.Conditions = append(out.Conditions, fromConditionProto(c))
	}
	return out
}

func toSubnet(sn network.Subnet) *networkv1.Subnet {
	return &networkv1.Subnet{
		Meta:   toMetaProto(sn.Meta),
		Spec:   toSubnetSpec(sn.Spec),
		Status: toSubnetStatusProto(sn.Status),
	}
}

func fromSubnet(sn *networkv1.Subnet) network.Subnet {
	return network.Subnet{
		Meta:   fromMetaProto(sn.GetMeta()),
		Spec:   fromSubnetSpec(sn.GetSpec()),
		Status: fromSubnetStatusProto(sn.GetStatus()),
	}
}

func toSubnetEvent(e network.SubnetEvent) *networkv1.SubnetEvent {
	out := &networkv1.SubnetEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case network.EventAdded:
		out.Type = networkv1.SubnetEvent_ADDED
	case network.EventModified:
		out.Type = networkv1.SubnetEvent_MODIFIED
	case network.EventDeleted:
		out.Type = networkv1.SubnetEvent_DELETED
	case network.EventBookmark:
		out.Type = networkv1.SubnetEvent_BOOKMARK
	}
	if out.Type != networkv1.SubnetEvent_BOOKMARK {
		out.Subnet = toSubnet(e.Object)
	}
	return out
}

func toConditionProto(c resource.Condition) *resourcev1.Condition {
	return &resourcev1.Condition{
		Type:             c.Type,
		Status:           string(c.Status),
		Reason:           c.Reason,
		Message:          c.Message,
		LastTransitionAt: timestamppb.New(c.LastTransitionAt),
	}
}

func fromConditionProto(c *resourcev1.Condition) resource.Condition {
	return resource.Condition{
		Type:             c.GetType(),
		Status:           resource.ConditionStatus(c.GetStatus()),
		Reason:           c.GetReason(),
		Message:          c.GetMessage(),
		LastTransitionAt: c.GetLastTransitionAt().AsTime(),
	}
}

func toMetaProto(m resource.ObjectMeta) *resourcev1.ObjectMeta {
	out := &resourcev1.ObjectMeta{
		Id:              m.ID,
		Name:            m.Name,
		TenantId:        m.TenantID,
		ResourceVersion: m.ResourceVersion,
		CreatedAt:       timestamppb.New(m.CreatedAt),
	}
	if m.DeletedAt != nil {
		out.DeletedAt = timestamppb.New(*m.DeletedAt)
	}
	return out
}

func fromMetaProto(m *resourcev1.ObjectMeta) resource.ObjectMeta {
	out := resource.ObjectMeta{
		ID:              m.GetId(),
		Name:            m.GetName(),
		TenantID:        m.GetTenantId(),
		ResourceVersion: m.GetResourceVersion(),
		CreatedAt:       m.GetCreatedAt().AsTime(),
	}
	if m.GetDeletedAt() != nil {
		t := m.GetDeletedAt().AsTime()
		out.DeletedAt = &t
	}
	return out
}
