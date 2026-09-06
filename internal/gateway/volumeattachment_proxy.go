package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	blockstoragev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

// VolumeAttachmentProxy implements
// blockstoragev1.VolumeAttachmentServiceServer by forwarding every call to a
// real block-storage backend, unchanged. See VolumeProxy's identical note.
type VolumeAttachmentProxy struct {
	blockstoragev1.UnimplementedVolumeAttachmentServiceServer
	backend blockstoragev1.VolumeAttachmentServiceClient
}

func NewVolumeAttachmentProxy(backend blockstoragev1.VolumeAttachmentServiceClient) *VolumeAttachmentProxy {
	return &VolumeAttachmentProxy{backend: backend}
}

func (p *VolumeAttachmentProxy) Create(ctx context.Context, req *blockstoragev1.CreateVolumeAttachmentRequest) (*blockstoragev1.VolumeAttachment, error) {
	return p.backend.Create(ctx, req)
}

func (p *VolumeAttachmentProxy) Get(ctx context.Context, req *blockstoragev1.GetVolumeAttachmentRequest) (*blockstoragev1.VolumeAttachment, error) {
	return p.backend.Get(ctx, req)
}

func (p *VolumeAttachmentProxy) List(ctx context.Context, req *blockstoragev1.ListVolumeAttachmentsRequest) (*blockstoragev1.ListVolumeAttachmentsResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *VolumeAttachmentProxy) Delete(ctx context.Context, req *blockstoragev1.DeleteVolumeAttachmentRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *VolumeAttachmentProxy) Watch(req *blockstoragev1.WatchVolumeAttachmentsRequest, stream blockstoragev1.VolumeAttachmentService_WatchServer) error {
	backendStream, err := p.backend.Watch(stream.Context(), req)
	if err != nil {
		return err
	}
	for {
		ev, err := backendStream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
}
