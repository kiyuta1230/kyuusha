package grpcserver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/resource"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

// HostAggregateServer implements HostAggregateServiceServer -- its own type
// for the same Get/List/Watch signature clash HypervisorServer's doc
// comment explains. Admin-only by way of api-gateway's authz: its requests
// carry no tenant_id, which only admin/compute-admin roles may send.
type HostAggregateServer struct {
	computev1.UnimplementedHostAggregateServiceServer
	svc *compute.Service
}

func NewHostAggregateServer(svc *compute.Service) *HostAggregateServer {
	return &HostAggregateServer{svc: svc}
}

func (s *HostAggregateServer) Create(ctx context.Context, req *computev1.CreateHostAggregateRequest) (*computev1.HostAggregate, error) {
	a, err := s.svc.CreateHostAggregate(ctx, req.GetName(), fromHostAggregateSpec(req.GetSpec()))
	if err != nil {
		return nil, toHostAggregateStatus(err)
	}
	return toHostAggregate(*a), nil
}

func (s *HostAggregateServer) Get(ctx context.Context, req *computev1.GetHostAggregateRequest) (*computev1.HostAggregate, error) {
	a, err := s.svc.GetHostAggregate(ctx, req.GetId())
	if err != nil {
		return nil, toHostAggregateStatus(err)
	}
	return toHostAggregate(*a), nil
}

func (s *HostAggregateServer) List(ctx context.Context, _ *computev1.ListHostAggregatesRequest) (*computev1.ListHostAggregatesResponse, error) {
	as, err := s.svc.ListHostAggregates(ctx)
	if err != nil {
		return nil, toHostAggregateStatus(err)
	}
	out := &computev1.ListHostAggregatesResponse{}
	for _, a := range as {
		out.Items = append(out.Items, toHostAggregate(a))
	}
	return out, nil
}

func (s *HostAggregateServer) Update(ctx context.Context, req *computev1.UpdateHostAggregateRequest) (*computev1.HostAggregate, error) {
	in := req.GetHostAggregate()
	a := compute.HostAggregate{
		Meta: resource.ObjectMeta{
			ID:              in.GetMeta().GetId(),
			Name:            in.GetMeta().GetName(),
			ResourceVersion: in.GetMeta().GetResourceVersion(),
			CreatedAt:       in.GetMeta().GetCreatedAt().AsTime(),
			Finalizers:      fromFinalizersProto(in.GetMeta().GetFinalizers()),
		},
		Spec: fromHostAggregateSpec(in.GetSpec()),
	}
	out, err := s.svc.UpdateHostAggregate(ctx, &a)
	if err != nil {
		return nil, toHostAggregateStatus(err)
	}
	return toHostAggregate(*out), nil
}

func (s *HostAggregateServer) Delete(ctx context.Context, req *computev1.DeleteHostAggregateRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteHostAggregate(ctx, req.GetId()); err != nil {
		return nil, toHostAggregateStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *HostAggregateServer) Watch(req *computev1.WatchHostAggregatesRequest, stream computev1.HostAggregateService_WatchServer) error {
	events, err := s.svc.WatchHostAggregates(stream.Context(), req.GetSinceResourceVersion())
	if err != nil {
		return toHostAggregateStatus(err)
	}
	for e := range events {
		out := &computev1.HostAggregateEvent{ResourceVersion: e.ResourceVersion}
		switch e.Type {
		case compute.EventAdded:
			out.Type = computev1.HostAggregateEvent_ADDED
		case compute.EventModified:
			out.Type = computev1.HostAggregateEvent_MODIFIED
		case compute.EventDeleted:
			out.Type = computev1.HostAggregateEvent_DELETED
		case compute.EventBookmark:
			out.Type = computev1.HostAggregateEvent_BOOKMARK
		}
		if out.Type != computev1.HostAggregateEvent_BOOKMARK {
			out.HostAggregate = toHostAggregate(e.Object)
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func fromHostAggregateSpec(s *computev1.HostAggregateSpec) compute.HostAggregateSpec {
	return compute.HostAggregateSpec{Zone: s.GetZone(), Labels: s.GetLabels(), Hypervisors: s.GetHypervisors()}
}

func toHostAggregate(a compute.HostAggregate) *computev1.HostAggregate {
	meta := &resourcev1.ObjectMeta{
		Id:              a.Meta.ID,
		Name:            a.Meta.Name,
		ResourceVersion: a.Meta.ResourceVersion,
		CreatedAt:       timestamppb.New(a.Meta.CreatedAt),
		Finalizers:      toFinalizersProto(a.Meta.Finalizers),
	}
	if a.Meta.DeletedAt != nil {
		meta.DeletedAt = timestamppb.New(*a.Meta.DeletedAt)
	}
	return &computev1.HostAggregate{
		Meta: meta,
		Spec: &computev1.HostAggregateSpec{Zone: a.Spec.Zone, Labels: a.Spec.Labels, Hypervisors: a.Spec.Hypervisors},
	}
}

func toHostAggregateStatus(err error) error {
	switch {
	case errors.Is(err, compute.ErrHostAggregateNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, compute.ErrHostAggregateConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, compute.ErrHostAggregateHistoryPruned):
		return status.Error(codes.OutOfRange, err.Error())
	}
	return toStatus(err)
}
