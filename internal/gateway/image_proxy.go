package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

// ImageProxy implements imagev1.ImageServiceServer by forwarding every call
// to a real image backend, unchanged. Unlike Hypervisor, every Image RPC
// (including Create) is externally exposed -- KaaS controllers or operators
// publish Image records through this same path.
type ImageProxy struct {
	imagev1.UnimplementedImageServiceServer
	backend imagev1.ImageServiceClient
}

func NewImageProxy(backend imagev1.ImageServiceClient) *ImageProxy {
	return &ImageProxy{backend: backend}
}

func (p *ImageProxy) Create(ctx context.Context, req *imagev1.CreateImageRequest) (*imagev1.Image, error) {
	return p.backend.Create(ctx, req)
}

func (p *ImageProxy) Get(ctx context.Context, req *imagev1.GetImageRequest) (*imagev1.Image, error) {
	return p.backend.Get(ctx, req)
}

func (p *ImageProxy) List(ctx context.Context, req *imagev1.ListImagesRequest) (*imagev1.ListImagesResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *ImageProxy) Delete(ctx context.Context, req *imagev1.DeleteImageRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *ImageProxy) SetVisibility(ctx context.Context, req *imagev1.SetImageVisibilityRequest) (*imagev1.Image, error) {
	return p.backend.SetVisibility(ctx, req)
}

func (p *ImageProxy) Watch(req *imagev1.WatchImagesRequest, stream imagev1.ImageService_WatchServer) error {
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
