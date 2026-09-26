package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// NetworkInterfaceProxy implements networkv1.NetworkInterfaceServiceServer
// by forwarding every call to a real network backend, unchanged. See
// SubnetProxy's identical note.
type NetworkInterfaceProxy struct {
	networkv1.UnimplementedNetworkInterfaceServiceServer
	backend networkv1.NetworkInterfaceServiceClient
}

func NewNetworkInterfaceProxy(backend networkv1.NetworkInterfaceServiceClient) *NetworkInterfaceProxy {
	return &NetworkInterfaceProxy{backend: backend}
}

func (p *NetworkInterfaceProxy) Create(ctx context.Context, req *networkv1.CreateNetworkInterfaceRequest) (*networkv1.NetworkInterface, error) {
	return p.backend.Create(ctx, req)
}

func (p *NetworkInterfaceProxy) Get(ctx context.Context, req *networkv1.GetNetworkInterfaceRequest) (*networkv1.NetworkInterface, error) {
	return p.backend.Get(ctx, req)
}

func (p *NetworkInterfaceProxy) List(ctx context.Context, req *networkv1.ListNetworkInterfacesRequest) (*networkv1.ListNetworkInterfacesResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *NetworkInterfaceProxy) Update(ctx context.Context, req *networkv1.UpdateNetworkInterfaceRequest) (*networkv1.NetworkInterface, error) {
	return p.backend.Update(ctx, req)
}

func (p *NetworkInterfaceProxy) UpdateFirewallRules(ctx context.Context, req *networkv1.UpdateFirewallRulesRequest) (*networkv1.NetworkInterface, error) {
	return p.backend.UpdateFirewallRules(ctx, req)
}

func (p *NetworkInterfaceProxy) Delete(ctx context.Context, req *networkv1.DeleteNetworkInterfaceRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *NetworkInterfaceProxy) Watch(req *networkv1.WatchNetworkInterfacesRequest, stream networkv1.NetworkInterfaceService_WatchServer) error {
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
