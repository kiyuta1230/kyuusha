package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// SubnetProxy implements networkv1.SubnetServiceServer by forwarding every
// call to a real network backend, unchanged. Every RPC (including Create)
// is externally exposed, tenant-scoped -- see ImageProxy's identical note.
type SubnetProxy struct {
	networkv1.UnimplementedSubnetServiceServer
	backend networkv1.SubnetServiceClient
}

func NewSubnetProxy(backend networkv1.SubnetServiceClient) *SubnetProxy {
	return &SubnetProxy{backend: backend}
}

func (p *SubnetProxy) Create(ctx context.Context, req *networkv1.CreateSubnetRequest) (*networkv1.Subnet, error) {
	return p.backend.Create(ctx, req)
}

func (p *SubnetProxy) Get(ctx context.Context, req *networkv1.GetSubnetRequest) (*networkv1.Subnet, error) {
	return p.backend.Get(ctx, req)
}

func (p *SubnetProxy) List(ctx context.Context, req *networkv1.ListSubnetsRequest) (*networkv1.ListSubnetsResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *SubnetProxy) Update(ctx context.Context, req *networkv1.UpdateSubnetRequest) (*networkv1.Subnet, error) {
	return p.backend.Update(ctx, req)
}

func (p *SubnetProxy) Delete(ctx context.Context, req *networkv1.DeleteSubnetRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *SubnetProxy) Watch(req *networkv1.WatchSubnetsRequest, stream networkv1.SubnetService_WatchServer) error {
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
