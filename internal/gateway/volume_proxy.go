package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

// VolumeProxy implements blockstoragev1.VolumeServiceServer by forwarding
// every call to a real block-storage backend, unchanged. Every RPC
// (including Create) is externally exposed, tenant-scoped -- see
// ImageProxy's identical note.
type VolumeProxy struct {
	blockstoragev1.UnimplementedVolumeServiceServer
	backend blockstoragev1.VolumeServiceClient
}

func NewVolumeProxy(backend blockstoragev1.VolumeServiceClient) *VolumeProxy {
	return &VolumeProxy{backend: backend}
}

func (p *VolumeProxy) Create(ctx context.Context, req *blockstoragev1.CreateVolumeRequest) (*blockstoragev1.Volume, error) {
	return p.backend.Create(ctx, req)
}

func (p *VolumeProxy) Get(ctx context.Context, req *blockstoragev1.GetVolumeRequest) (*blockstoragev1.Volume, error) {
	return p.backend.Get(ctx, req)
}

func (p *VolumeProxy) List(ctx context.Context, req *blockstoragev1.ListVolumesRequest) (*blockstoragev1.ListVolumesResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *VolumeProxy) Delete(ctx context.Context, req *blockstoragev1.DeleteVolumeRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *VolumeProxy) Watch(req *blockstoragev1.WatchVolumesRequest, stream blockstoragev1.VolumeService_WatchServer) error {
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
