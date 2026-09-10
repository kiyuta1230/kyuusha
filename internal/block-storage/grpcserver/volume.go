// Package grpcserver adapts internal/block-storage.Service to the generated
// VolumeServiceServer/VolumeAttachmentServiceServer interfaces. It only
// translates between wire types and domain types; all behavior lives in
// blockstorage.Service.
package grpcserver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	blockstorage "gitlab.com/ki.yuta1230/kyuusha/internal/block-storage"
	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"

	blockstoragev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	resourcev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

type VolumeServer struct {
	blockstoragev1.UnimplementedVolumeServiceServer
	svc *blockstorage.Service
}

func NewVolumeServer(svc *blockstorage.Service) *VolumeServer {
	return &VolumeServer{svc: svc}
}

func (s *VolumeServer) Create(ctx context.Context, req *blockstoragev1.CreateVolumeRequest) (*blockstoragev1.Volume, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	vol, err := s.svc.CreateVolume(ctx, req.GetTenantId(), req.GetName(), fromVolumeSpec(req.GetSpec()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toVolume(*vol), nil
}

func (s *VolumeServer) Get(ctx context.Context, req *blockstoragev1.GetVolumeRequest) (*blockstoragev1.Volume, error) {
	vol, err := s.svc.GetVolume(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toVolume(*vol), nil
}

func (s *VolumeServer) List(ctx context.Context, req *blockstoragev1.ListVolumesRequest) (*blockstoragev1.ListVolumesResponse, error) {
	vols, err := s.svc.ListVolumes(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &blockstoragev1.ListVolumesResponse{}
	for _, vol := range vols {
		out.Items = append(out.Items, toVolume(vol))
	}
	return out, nil
}

func (s *VolumeServer) Delete(ctx context.Context, req *blockstoragev1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteVolume(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *VolumeServer) Watch(req *blockstoragev1.WatchVolumesRequest, stream blockstoragev1.VolumeService_WatchServer) error {
	events, err := s.svc.WatchVolumes(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		if err := stream.Send(toVolumeEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, blockstorage.ErrVolumeNotFound), errors.Is(err, blockstorage.ErrVolumeAttachmentNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, blockstorage.ErrVolumeConflict), errors.Is(err, blockstorage.ErrVolumeAttachmentConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, blockstorage.ErrVolumeHistoryPruned), errors.Is(err, blockstorage.ErrVolumeAttachmentHistoryPruned):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, blockstorage.ErrQuotaExceeded):
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func fromVolumeSpec(s *blockstoragev1.VolumeSpec) blockstorage.VolumeSpec {
	return blockstorage.VolumeSpec{
		SizeGB:            s.GetSizeGb(),
		Protocol:          fromStorageProtocol(s.GetProtocol()),
		StorageConnection: s.GetStorageConnection(),
		Identifier:        s.GetIdentifier(),
	}
}

func toVolumeSpec(s blockstorage.VolumeSpec) *blockstoragev1.VolumeSpec {
	return &blockstoragev1.VolumeSpec{
		SizeGb:            s.SizeGB,
		Protocol:          toStorageProtocol(s.Protocol),
		StorageConnection: s.StorageConnection,
		Identifier:        s.Identifier,
	}
}

func fromStorageProtocol(p blockstoragev1.StorageProtocol) blockstorage.StorageProtocol {
	switch p {
	case blockstoragev1.StorageProtocol_ISCSI:
		return blockstorage.StorageProtocolISCSI
	case blockstoragev1.StorageProtocol_NVME_OF:
		return blockstorage.StorageProtocolNVMeOF
	case blockstoragev1.StorageProtocol_NFS:
		return blockstorage.StorageProtocolNFS
	default:
		return ""
	}
}

func toStorageProtocol(p blockstorage.StorageProtocol) blockstoragev1.StorageProtocol {
	switch p {
	case blockstorage.StorageProtocolISCSI:
		return blockstoragev1.StorageProtocol_ISCSI
	case blockstorage.StorageProtocolNVMeOF:
		return blockstoragev1.StorageProtocol_NVME_OF
	case blockstorage.StorageProtocolNFS:
		return blockstoragev1.StorageProtocol_NFS
	default:
		return blockstoragev1.StorageProtocol_STORAGE_PROTOCOL_UNSPECIFIED
	}
}

func toVolumeStatusProto(st blockstorage.VolumeStatus) *blockstoragev1.VolumeStatus {
	out := &blockstoragev1.VolumeStatus{Phase: string(st.Phase)}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, toConditionProto(c))
	}
	return out
}

func toVolume(vol blockstorage.Volume) *blockstoragev1.Volume {
	return &blockstoragev1.Volume{
		Meta:   toMetaProto(vol.Meta),
		Spec:   toVolumeSpec(vol.Spec),
		Status: toVolumeStatusProto(vol.Status),
	}
}

func toVolumeEvent(e blockstorage.VolumeEvent) *blockstoragev1.VolumeEvent {
	out := &blockstoragev1.VolumeEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case blockstorage.EventAdded:
		out.Type = blockstoragev1.VolumeEvent_ADDED
	case blockstorage.EventModified:
		out.Type = blockstoragev1.VolumeEvent_MODIFIED
	case blockstorage.EventDeleted:
		out.Type = blockstoragev1.VolumeEvent_DELETED
	case blockstorage.EventBookmark:
		out.Type = blockstoragev1.VolumeEvent_BOOKMARK
	}
	if out.Type != blockstoragev1.VolumeEvent_BOOKMARK {
		out.Volume = toVolume(e.Object)
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

func toFinalizersProto(fs []resource.Finalizer) []*resourcev1.Finalizer {
	out := make([]*resourcev1.Finalizer, len(fs))
	for i, f := range fs {
		out[i] = &resourcev1.Finalizer{Name: f.Name, AddedBy: f.AddedBy}
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
	}
	if m.DeletedAt != nil {
		out.DeletedAt = timestamppb.New(*m.DeletedAt)
	}
	return out
}
