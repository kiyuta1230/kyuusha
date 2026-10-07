package grpcserver

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/resource"
)

type IPReservationServer struct {
	networkv1.UnimplementedIPReservationServiceServer
	svc *network.Service
}

func NewIPReservationServer(svc *network.Service) *IPReservationServer {
	return &IPReservationServer{svc: svc}
}

func (s *IPReservationServer) Create(ctx context.Context, req *networkv1.CreateIPReservationRequest) (*networkv1.IPReservation, error) {
	sp := req.GetSpec()
	r, err := s.svc.CreateIPReservation(ctx, req.GetTenantId(), req.GetName(), network.IPReservationSpec{
		NetworkID: sp.GetNetworkId(), Zone: sp.GetZone(), SubnetID: sp.GetSubnetId(), RequestedAddresses: sp.GetRequestedAddresses(),
	}, resource.Metadata{Labels: req.GetLabels(), Annotations: req.GetAnnotations()})
	if err != nil {
		return nil, toStatus(err)
	}
	return toIPReservation(*r), nil
}

func (s *IPReservationServer) Get(ctx context.Context, req *networkv1.GetIPReservationRequest) (*networkv1.IPReservation, error) {
	r, err := s.svc.GetIPReservation(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toIPReservation(*r), nil
}

func (s *IPReservationServer) List(ctx context.Context, req *networkv1.ListIPReservationsRequest) (*networkv1.ListIPReservationsResponse, error) {
	rs, err := s.svc.ListIPReservations(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListIPReservationsResponse{}
	for _, r := range rs {
		out.Items = append(out.Items, toIPReservation(r))
	}
	return out, nil
}

func (s *IPReservationServer) Update(ctx context.Context, req *networkv1.UpdateIPReservationRequest) (*networkv1.IPReservation, error) {
	r := network.IPReservation{Meta: fromMetaProto(req.GetIpReservation().GetMeta())}
	out, err := s.svc.UpdateIPReservation(ctx, req.GetTenantId(), &r)
	if err != nil {
		return nil, toStatus(err)
	}
	return toIPReservation(*out), nil
}

func (s *IPReservationServer) Delete(ctx context.Context, req *networkv1.DeleteIPReservationRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteIPReservation(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *IPReservationServer) Watch(req *networkv1.WatchIPReservationsRequest, stream networkv1.IPReservationService_WatchServer) error {
	events, err := s.svc.WatchIPReservations(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		out := &networkv1.IPReservationEvent{Type: networkv1.IPReservationEvent_Type(eventType(e.Type)), ResourceVersion: e.ResourceVersion}
		if e.Type != network.EventBookmark {
			out.IpReservation = toIPReservation(e.Object)
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func toIPReservation(r network.IPReservation) *networkv1.IPReservation {
	var conds []*resourcev1.Condition
	for _, c := range r.Status.Conditions {
		conds = append(conds, toConditionProto(c))
	}
	return &networkv1.IPReservation{
		Meta: toMetaProto(r.Meta),
		Spec: &networkv1.IPReservationSpec{NetworkId: r.Spec.NetworkID, Zone: r.Spec.Zone, SubnetId: r.Spec.SubnetID, RequestedAddresses: r.Spec.RequestedAddresses},
		Status: &networkv1.IPReservationStatus{
			Phase: string(r.Status.Phase), Conditions: conds,
			Addresses: r.Status.Addresses, SubnetId: r.Status.SubnetID, Zone: r.Status.Zone,
		},
	}
}
