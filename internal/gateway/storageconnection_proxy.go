package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

// StorageConnectionProxy forwards every call to a real block-storage
// backend, unchanged. None of Create/Get/List/Delete/WatchStorageConnections
// carry a tenant_id (StorageConnection isn't tenant-scoped, same as
// Hypervisor -- see HypervisorProxy's identical note), which makes all of
// them admin-only through api-gateway via internal/authz's rule for
// requests without one: registering which Availability Zones a storage
// backend may be connected from is an operator/storage-admin concern, not
// something ordinary KaaS tenants should see or change.
type StorageConnectionProxy struct {
	blockstoragev1.UnimplementedStorageConnectionServiceServer
	backend blockstoragev1.StorageConnectionServiceClient
}

func NewStorageConnectionProxy(backend blockstoragev1.StorageConnectionServiceClient) *StorageConnectionProxy {
	return &StorageConnectionProxy{backend: backend}
}

func (p *StorageConnectionProxy) Create(ctx context.Context, req *blockstoragev1.CreateStorageConnectionRequest) (*blockstoragev1.StorageConnection, error) {
	return p.backend.Create(ctx, req)
}

func (p *StorageConnectionProxy) Get(ctx context.Context, req *blockstoragev1.GetStorageConnectionRequest) (*blockstoragev1.StorageConnection, error) {
	return p.backend.Get(ctx, req)
}

func (p *StorageConnectionProxy) List(ctx context.Context, req *blockstoragev1.ListStorageConnectionsRequest) (*blockstoragev1.ListStorageConnectionsResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *StorageConnectionProxy) Delete(ctx context.Context, req *blockstoragev1.DeleteStorageConnectionRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *StorageConnectionProxy) Watch(req *blockstoragev1.WatchStorageConnectionsRequest, stream blockstoragev1.StorageConnectionService_WatchServer) error {
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
