// Package grpcserver adapts internal/image.Service to the generated
// ImageServiceServer interface. It only translates between wire types and
// domain types; all behavior lives in image.Service.
package grpcserver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"gitlab.com/ki.yuta1230/kyuusha/internal/image"

	imagev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/image/v1"
	resourcev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

type Server struct {
	imagev1.UnimplementedImageServiceServer
	svc *image.Service
}

func New(svc *image.Service) *Server {
	return &Server{svc: svc}
}

func (s *Server) Create(ctx context.Context, req *imagev1.CreateImageRequest) (*imagev1.Image, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	img, err := s.svc.Create(ctx, req.GetTenantId(), req.GetName(), fromSpec(req.GetSpec()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toImage(*img), nil
}

func (s *Server) Get(ctx context.Context, req *imagev1.GetImageRequest) (*imagev1.Image, error) {
	img, err := s.svc.Get(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toImage(*img), nil
}

func (s *Server) List(ctx context.Context, req *imagev1.ListImagesRequest) (*imagev1.ListImagesResponse, error) {
	imgs, err := s.svc.List(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &imagev1.ListImagesResponse{}
	for _, img := range imgs {
		out.Items = append(out.Items, toImage(img))
	}
	return out, nil
}

func (s *Server) Delete(ctx context.Context, req *imagev1.DeleteImageRequest) (*emptypb.Empty, error) {
	if err := s.svc.Delete(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) Watch(req *imagev1.WatchImagesRequest, stream imagev1.ImageService_WatchServer) error {
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
	switch {
	case errors.Is(err, image.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, image.ErrConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, image.ErrHistoryPruned):
		return status.Error(codes.OutOfRange, err.Error())
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func fromArtifact(a *imagev1.ImageArtifact) image.Artifact {
	return image.Artifact{URL: a.GetUrl(), Digest: a.GetDigest()}
}

func toArtifact(a image.Artifact) *imagev1.ImageArtifact {
	return &imagev1.ImageArtifact{Url: a.URL, Digest: a.Digest}
}

func fromFormat(f imagev1.ImageFormat) image.Format {
	switch f {
	case imagev1.ImageFormat_KERNEL_ROOTFS:
		return image.FormatKernelRootfs
	case imagev1.ImageFormat_QCOW2:
		return image.FormatQCOW2
	default:
		return image.FormatUnspecified
	}
}

func toFormat(f image.Format) imagev1.ImageFormat {
	switch f {
	case image.FormatKernelRootfs:
		return imagev1.ImageFormat_KERNEL_ROOTFS
	case image.FormatQCOW2:
		return imagev1.ImageFormat_QCOW2
	default:
		return imagev1.ImageFormat_IMAGE_FORMAT_UNSPECIFIED
	}
}

func fromSpec(s *imagev1.ImageSpec) image.Spec {
	return image.Spec{
		Format:   fromFormat(s.GetFormat()),
		Kernel:   fromArtifact(s.GetKernel()),
		Rootfs:   fromArtifact(s.GetRootfs()),
		Disk:     fromArtifact(s.GetDisk()),
		BootArgs: s.GetBootArgs(),
	}
}

func toSpec(s image.Spec) *imagev1.ImageSpec {
	return &imagev1.ImageSpec{
		Format:   toFormat(s.Format),
		Kernel:   toArtifact(s.Kernel),
		Rootfs:   toArtifact(s.Rootfs),
		Disk:     toArtifact(s.Disk),
		BootArgs: s.BootArgs,
	}
}

func toStatusProto(st image.Status) *imagev1.ImageStatus {
	out := &imagev1.ImageStatus{Phase: string(st.Phase), SizeBytes: st.SizeBytes}
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

func toImage(img image.Image) *imagev1.Image {
	meta := &resourcev1.ObjectMeta{
		Id:              img.Meta.ID,
		Name:            img.Meta.Name,
		TenantId:        img.Meta.TenantID,
		ResourceVersion: img.Meta.ResourceVersion,
		CreatedAt:       timestamppb.New(img.Meta.CreatedAt),
	}
	if img.Meta.DeletedAt != nil {
		meta.DeletedAt = timestamppb.New(*img.Meta.DeletedAt)
	}
	return &imagev1.Image{
		Meta:   meta,
		Spec:   toSpec(img.Spec),
		Status: toStatusProto(img.Status),
	}
}

func toEvent(e image.Event) *imagev1.ImageEvent {
	out := &imagev1.ImageEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case image.EventAdded:
		out.Type = imagev1.ImageEvent_ADDED
	case image.EventModified:
		out.Type = imagev1.ImageEvent_MODIFIED
	case image.EventDeleted:
		out.Type = imagev1.ImageEvent_DELETED
	case image.EventBookmark:
		out.Type = imagev1.ImageEvent_BOOKMARK
	}
	if out.Type != imagev1.ImageEvent_BOOKMARK {
		out.Image = toImage(e.Object)
	}
	return out
}
