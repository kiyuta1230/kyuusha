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

	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/resource"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
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
	sn, err := s.svc.CreateSubnetWithMetadata(ctx, req.GetTenantId(), req.GetName(), fromSubnetSpec(req.GetSpec()),
		resource.Metadata{Labels: req.GetLabels(), Annotations: req.GetAnnotations()})
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

// SetStatusValues is the admin-only correction of a Subnet's
// system-written values: authz lets the tenant itself through (it's the
// tenant's own Subnet), so the admin check is here.
func (s *SubnetServer) SetStatusValues(ctx context.Context, req *networkv1.SetSubnetStatusValuesRequest) (*networkv1.Subnet, error) {
	if !authn.CallerIsAdminFromContext(ctx) {
		return nil, status.Error(codes.PermissionDenied, "only an admin may correct system-written values")
	}
	sn, err := s.svc.SetSubnetStatusValues(ctx, req.GetTenantId(), req.GetId(), req.GetValues(), req.GetAttributes())
	if err != nil {
		return nil, toStatus(err)
	}
	return toSubnet(*sn), nil
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, network.ErrSubnetNotFound), errors.Is(err, network.ErrNetworkInterfaceNotFound),
		errors.Is(err, network.ErrNetworkNotFound), errors.Is(err, network.ErrNetworkClassNotFound), errors.Is(err, network.ErrAllocationPoolNotFound), errors.Is(err, network.ErrSecurityGroupNotFound), errors.Is(err, network.ErrIPReservationNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, network.ErrSubnetConflict), errors.Is(err, network.ErrNetworkInterfaceConflict),
		errors.Is(err, network.ErrNetworkConflict), errors.Is(err, network.ErrNetworkClassConflict), errors.Is(err, network.ErrAllocationPoolConflict), errors.Is(err, network.ErrSecurityGroupConflict), errors.Is(err, network.ErrIPReservationConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, network.ErrSubnetHistoryPruned), errors.Is(err, network.ErrNetworkInterfaceHistoryPruned),
		errors.Is(err, network.ErrNetworkHistoryPruned), errors.Is(err, network.ErrNetworkClassHistoryPruned), errors.Is(err, network.ErrAllocationPoolHistoryPruned), errors.Is(err, network.ErrSecurityGroupHistoryPruned), errors.Is(err, network.ErrIPReservationHistoryPruned):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, network.ErrInUse):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, network.ErrAdmissionDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, network.ErrAdmissionUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func fromAddresses(as []*networkv1.SubnetAddress) []network.SubnetAddress {
	var out []network.SubnetAddress
	for _, a := range as {
		out = append(out, network.SubnetAddress{CIDR: a.GetCidr(), GatewayIP: a.GetGatewayIp()})
	}
	return out
}

func toAddresses(as []network.SubnetAddress) []*networkv1.SubnetAddress {
	var out []*networkv1.SubnetAddress
	for _, a := range as {
		out = append(out, &networkv1.SubnetAddress{Cidr: a.CIDR, GatewayIp: a.GatewayIP})
	}
	return out
}

func fromSubnetSpec(s *networkv1.SubnetSpec) network.SubnetSpec {
	return network.SubnetSpec{
		NetworkID:           s.GetNetworkId(),
		Zone:                s.GetZone(),
		RequestedAddresses:  fromAddresses(s.GetRequestedAddresses()),
		DNSServers:          s.GetDnsServers(),
		AllocatableIPRanges: s.GetAllocatableIpRanges(),
	}
}

func toSubnetSpec(s network.SubnetSpec) *networkv1.SubnetSpec {
	return &networkv1.SubnetSpec{
		NetworkId:           s.NetworkID,
		Zone:                s.Zone,
		RequestedAddresses:  toAddresses(s.RequestedAddresses),
		DnsServers:          s.DNSServers,
		AllocatableIpRanges: s.AllocatableIPRanges,
	}
}

func toSubnetStatusProto(st network.SubnetStatus) *networkv1.SubnetStatus {
	out := &networkv1.SubnetStatus{
		Phase: string(st.Phase), Addresses: toAddresses(st.Addresses), Values: st.Values,
		Attributes: st.Attributes, Allocations: toAllocationsProto(st.Allocations),
	}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, toConditionProto(c))
	}
	return out
}

func fromSubnetStatusProto(st *networkv1.SubnetStatus) network.SubnetStatus {
	out := network.SubnetStatus{
		Phase: network.SubnetPhase(st.GetPhase()), Addresses: fromAddresses(st.GetAddresses()), Values: st.GetValues(),
		Attributes: st.GetAttributes(), Allocations: fromAllocationsProto(st.GetAllocations()),
	}
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

func toFinalizersProto(fs []resource.Finalizer) []*resourcev1.Finalizer {
	out := make([]*resourcev1.Finalizer, len(fs))
	for i, f := range fs {
		out[i] = &resourcev1.Finalizer{Name: f.Name, AddedBy: f.AddedBy}
	}
	return out
}

func fromFinalizersProto(fs []*resourcev1.Finalizer) []resource.Finalizer {
	out := make([]resource.Finalizer, len(fs))
	for i, f := range fs {
		out[i] = resource.Finalizer{Name: f.GetName(), AddedBy: f.GetAddedBy()}
	}
	return out
}

func toMetaProto(m resource.ObjectMeta) *resourcev1.ObjectMeta {
	out := &resourcev1.ObjectMeta{
		Id:              m.ID,
		Name:            m.Name,
		TenantId:        m.TenantID,
		ResourceVersion: m.ResourceVersion,
		CreatedAt:       timestamppb.New(m.CreatedAt),
		Finalizers:      toFinalizersProto(m.Finalizers),
		Labels:          m.Labels,
		Annotations:     m.Annotations,
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
		Finalizers:      fromFinalizersProto(m.GetFinalizers()),
		Labels:          m.GetLabels(),
		Annotations:     m.GetAnnotations(),
	}
	if m.GetDeletedAt() != nil {
		t := m.GetDeletedAt().AsTime()
		out.DeletedAt = &t
	}
	return out
}
