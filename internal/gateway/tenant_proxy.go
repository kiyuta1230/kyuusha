package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

// TenantProxy implements identityv1.TenantServiceServer by forwarding every
// call to a real identity backend, unchanged. See VirtualMachineProxy's doc
// comment: same pattern, same "revisit once several backends exist" note.
type TenantProxy struct {
	identityv1.UnimplementedTenantServiceServer
	backend identityv1.TenantServiceClient
}

func NewTenantProxy(backend identityv1.TenantServiceClient) *TenantProxy {
	return &TenantProxy{backend: backend}
}

func (p *TenantProxy) Create(ctx context.Context, req *identityv1.CreateTenantRequest) (*identityv1.Tenant, error) {
	return p.backend.Create(ctx, req)
}

func (p *TenantProxy) Get(ctx context.Context, req *identityv1.GetTenantRequest) (*identityv1.Tenant, error) {
	return p.backend.Get(ctx, req)
}

func (p *TenantProxy) List(ctx context.Context, req *identityv1.ListTenantsRequest) (*identityv1.ListTenantsResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *TenantProxy) Update(ctx context.Context, req *identityv1.UpdateTenantRequest) (*identityv1.Tenant, error) {
	return p.backend.Update(ctx, req)
}

func (p *TenantProxy) Delete(ctx context.Context, req *identityv1.DeleteTenantRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *TenantProxy) Watch(req *identityv1.WatchTenantsRequest, stream identityv1.TenantService_WatchServer) error {
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
