// Package grpcserver adapts internal/identity.Service to the generated
// TenantServiceServer interface. It only translates between wire types and
// domain types; all behavior lives in identity.Service.
package grpcserver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kiyuta1230/kyuusha/internal/identity"
	"github.com/kiyuta1230/kyuusha/internal/resource"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

type Server struct {
	identityv1.UnimplementedTenantServiceServer
	svc *identity.Service
}

func New(svc *identity.Service) *Server {
	return &Server{svc: svc}
}

func (s *Server) Create(ctx context.Context, req *identityv1.CreateTenantRequest) (*identityv1.Tenant, error) {
	tn, err := s.svc.Create(ctx, req.GetName(), fromSpec(req.GetSpec()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toTenant(*tn), nil
}

func (s *Server) Get(ctx context.Context, req *identityv1.GetTenantRequest) (*identityv1.Tenant, error) {
	tn, err := s.svc.Get(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toTenant(*tn), nil
}

func (s *Server) List(ctx context.Context, req *identityv1.ListTenantsRequest) (*identityv1.ListTenantsResponse, error) {
	tenants, err := s.svc.List(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &identityv1.ListTenantsResponse{}
	for _, tn := range tenants {
		out.Items = append(out.Items, toTenant(tn))
	}
	return out, nil
}

func (s *Server) Update(ctx context.Context, req *identityv1.UpdateTenantRequest) (*identityv1.Tenant, error) {
	// req.TenantId (top-level) is what internal/authz actually authorized;
	// req.Tenant.Meta.TenantId must agree, or a caller authorized for their
	// own tenant could smuggle a mutation of another Tenant inside the
	// nested message (see compute/grpcserver's identical Update check).
	if req.GetTenantId() == "" || req.GetTenantId() != req.GetTenant().GetMeta().GetTenantId() {
		return nil, status.Error(codes.InvalidArgument, "tenant_id must be set and match tenant.meta.tenant_id")
	}
	tn := fromTenant(req.GetTenant())
	updated, err := s.svc.Update(ctx, &tn)
	if err != nil {
		return nil, toStatus(err)
	}
	return toTenant(*updated), nil
}

func (s *Server) Delete(ctx context.Context, req *identityv1.DeleteTenantRequest) (*emptypb.Empty, error) {
	if err := s.svc.Delete(ctx, req.GetTenantId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) Watch(req *identityv1.WatchTenantsRequest, stream identityv1.TenantService_WatchServer) error {
	events, err := s.svc.Watch(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		if err := stream.Send(toEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func toStatus(err error) error {
	switch err {
	case identity.ErrNotFound:
		return status.Error(codes.NotFound, err.Error())
	case identity.ErrConflict:
		return status.Error(codes.Aborted, err.Error())
	case identity.ErrHistoryPruned:
		return status.Error(codes.OutOfRange, err.Error())
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func fromSpec(s *identityv1.TenantSpec) identity.TenantSpec {
	return identity.TenantSpec{
		DisplayName: s.GetDisplayName(),
		Quota:       fromQuota(s.GetQuota()),
	}
}

func toSpec(s identity.TenantSpec) *identityv1.TenantSpec {
	return &identityv1.TenantSpec{
		DisplayName: s.DisplayName,
		Quota:       toQuota(s.Quota),
	}
}

func fromQuota(q *identityv1.QuotaSpec) identity.QuotaSpec {
	return identity.QuotaSpec{
		MaxVCPU:          q.GetMaxVcpu(),
		MaxMemoryMB:      q.GetMaxMemoryMb(),
		MaxVolumeGB:      q.GetMaxVolumeGb(),
		MaxVMs:           q.GetMaxVms(),
		MaxVCPUPerVM:     q.GetMaxVcpuPerVm(),
		MaxMemoryMBPerVM: q.GetMaxMemoryMbPerVm(),
	}
}

func toQuota(q identity.QuotaSpec) *identityv1.QuotaSpec {
	return &identityv1.QuotaSpec{
		MaxVcpu:          q.MaxVCPU,
		MaxMemoryMb:      q.MaxMemoryMB,
		MaxVolumeGb:      q.MaxVolumeGB,
		MaxVms:           q.MaxVMs,
		MaxVcpuPerVm:     q.MaxVCPUPerVM,
		MaxMemoryMbPerVm: q.MaxMemoryMBPerVM,
	}
}

func toStatusProto(st identity.TenantStatus) *identityv1.TenantStatus {
	out := &identityv1.TenantStatus{Phase: string(st.Phase)}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, &resourcev1.Condition{
			Type:             c.Type,
			Status:           string(c.Status),
			Reason:           c.Reason,
			Message:          c.Message,
			LastTransitionAt: timestamppb.New(c.LastTransitionAt),
		})
	}
	return out
}

func fromStatusProto(st *identityv1.TenantStatus) identity.TenantStatus {
	out := identity.TenantStatus{Phase: identity.Phase(st.GetPhase())}
	for _, c := range st.GetConditions() {
		out.Conditions = append(out.Conditions, resource.Condition{
			Type:             c.GetType(),
			Status:           resource.ConditionStatus(c.GetStatus()),
			Reason:           c.GetReason(),
			Message:          c.GetMessage(),
			LastTransitionAt: c.GetLastTransitionAt().AsTime(),
		})
	}
	return out
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

func toTenant(tn identity.Tenant) *identityv1.Tenant {
	meta := &resourcev1.ObjectMeta{
		Id:              tn.Meta.ID,
		Name:            tn.Meta.Name,
		TenantId:        tn.Meta.TenantID,
		ResourceVersion: tn.Meta.ResourceVersion,
		CreatedAt:       timestamppb.New(tn.Meta.CreatedAt),
		Finalizers:      toFinalizersProto(tn.Meta.Finalizers),
	}
	if tn.Meta.DeletedAt != nil {
		meta.DeletedAt = timestamppb.New(*tn.Meta.DeletedAt)
	}
	return &identityv1.Tenant{
		Meta:   meta,
		Spec:   toSpec(tn.Spec),
		Status: toStatusProto(tn.Status),
	}
}

func fromTenant(tn *identityv1.Tenant) identity.Tenant {
	meta := tn.GetMeta()
	out := identity.Tenant{
		Meta: resource.ObjectMeta{
			ID:              meta.GetId(),
			Name:            meta.GetName(),
			TenantID:        meta.GetTenantId(),
			ResourceVersion: meta.GetResourceVersion(),
			CreatedAt:       meta.GetCreatedAt().AsTime(),
			Finalizers:      fromFinalizersProto(meta.GetFinalizers()),
		},
		Spec:   fromSpec(tn.GetSpec()),
		Status: fromStatusProto(tn.GetStatus()),
	}
	if meta.GetDeletedAt() != nil {
		t := meta.GetDeletedAt().AsTime()
		out.Meta.DeletedAt = &t
	}
	return out
}

func toEvent(e identity.Event) *identityv1.TenantEvent {
	out := &identityv1.TenantEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case identity.EventAdded:
		out.Type = identityv1.TenantEvent_ADDED
	case identity.EventModified:
		out.Type = identityv1.TenantEvent_MODIFIED
	case identity.EventDeleted:
		out.Type = identityv1.TenantEvent_DELETED
	case identity.EventBookmark:
		out.Type = identityv1.TenantEvent_BOOKMARK
	}
	if out.Type != identityv1.TenantEvent_BOOKMARK {
		out.Tenant = toTenant(e.Object)
	}
	return out
}
