package grpcserver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	blockstorage "github.com/kiyuta1230/kyuusha/internal/block-storage"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

type VolumeAttachmentServer struct {
	blockstoragev1.UnimplementedVolumeAttachmentServiceServer
	svc *blockstorage.Service
}

func NewVolumeAttachmentServer(svc *blockstorage.Service) *VolumeAttachmentServer {
	return &VolumeAttachmentServer{svc: svc}
}

func (s *VolumeAttachmentServer) Create(ctx context.Context, req *blockstoragev1.CreateVolumeAttachmentRequest) (*blockstoragev1.VolumeAttachment, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	a, err := s.svc.CreateVolumeAttachment(ctx, req.GetTenantId(), req.GetName(), fromVolumeAttachmentSpec(req.GetSpec()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toVolumeAttachment(*a), nil
}

func (s *VolumeAttachmentServer) Get(ctx context.Context, req *blockstoragev1.GetVolumeAttachmentRequest) (*blockstoragev1.VolumeAttachment, error) {
	a, err := s.svc.GetVolumeAttachment(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toVolumeAttachment(*a), nil
}

func (s *VolumeAttachmentServer) List(ctx context.Context, req *blockstoragev1.ListVolumeAttachmentsRequest) (*blockstoragev1.ListVolumeAttachmentsResponse, error) {
	attachments, err := s.svc.ListVolumeAttachments(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &blockstoragev1.ListVolumeAttachmentsResponse{}
	for _, a := range attachments {
		out.Items = append(out.Items, toVolumeAttachment(a))
	}
	return out, nil
}

func (s *VolumeAttachmentServer) Delete(ctx context.Context, req *blockstoragev1.DeleteVolumeAttachmentRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteVolumeAttachment(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *VolumeAttachmentServer) Watch(req *blockstoragev1.WatchVolumeAttachmentsRequest, stream blockstoragev1.VolumeAttachmentService_WatchServer) error {
	events, err := s.svc.WatchVolumeAttachments(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		if err := stream.Send(toVolumeAttachmentEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func fromVolumeAttachmentSpec(s *blockstoragev1.VolumeAttachmentSpec) blockstorage.VolumeAttachmentSpec {
	return blockstorage.VolumeAttachmentSpec{
		VolumeID:   s.GetVolumeId(),
		VMID:       s.GetVmId(),
		DeviceHint: s.GetDeviceHint(),
	}
}

func toVolumeAttachmentSpec(s blockstorage.VolumeAttachmentSpec) *blockstoragev1.VolumeAttachmentSpec {
	return &blockstoragev1.VolumeAttachmentSpec{
		VolumeId:   s.VolumeID,
		VmId:       s.VMID,
		DeviceHint: s.DeviceHint,
	}
}

func toVolumeAttachmentStatusProto(st blockstorage.VolumeAttachmentStatus) *blockstoragev1.VolumeAttachmentStatus {
	out := &blockstoragev1.VolumeAttachmentStatus{
		Phase:      string(st.Phase),
		DevicePath: st.DevicePath,
		Hypervisor: st.Hypervisor,
	}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, toConditionProto(c))
	}
	return out
}

func toVolumeAttachment(a blockstorage.VolumeAttachment) *blockstoragev1.VolumeAttachment {
	return &blockstoragev1.VolumeAttachment{
		Meta:   toMetaProto(a.Meta),
		Spec:   toVolumeAttachmentSpec(a.Spec),
		Status: toVolumeAttachmentStatusProto(a.Status),
	}
}

func toVolumeAttachmentEvent(e blockstorage.VolumeAttachmentEvent) *blockstoragev1.VolumeAttachmentEvent {
	out := &blockstoragev1.VolumeAttachmentEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case blockstorage.EventAdded:
		out.Type = blockstoragev1.VolumeAttachmentEvent_ADDED
	case blockstorage.EventModified:
		out.Type = blockstoragev1.VolumeAttachmentEvent_MODIFIED
	case blockstorage.EventDeleted:
		out.Type = blockstoragev1.VolumeAttachmentEvent_DELETED
	case blockstorage.EventBookmark:
		out.Type = blockstoragev1.VolumeAttachmentEvent_BOOKMARK
	}
	if out.Type != blockstoragev1.VolumeAttachmentEvent_BOOKMARK {
		out.VolumeAttachment = toVolumeAttachment(e.Object)
	}
	return out
}
