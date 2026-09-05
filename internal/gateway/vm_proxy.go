// Package gateway implements api-gateway's side of docs/architecture.md
// "外部(KaaS)向けgRPCエンドポイント集約、認証、ルーティング": a thin,
// per-service reverse proxy. api-gateway is the only thing that terminates
// client JWTs and runs OPA; backend services (compute, ...) are reached only
// through it. Hand-written per RPC for now since there is exactly one
// backend service — worth revisiting for a generic passthrough once there
// are several.
package gateway

import (
	"context"
	"io"

	"google.golang.org/protobuf/types/known/emptypb"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

// VirtualMachineProxy implements computev1.VirtualMachineServiceServer by
// forwarding every call to a real compute backend, unchanged. Authn/authz
// happen in interceptors in front of this, not here.
type VirtualMachineProxy struct {
	computev1.UnimplementedVirtualMachineServiceServer
	backend computev1.VirtualMachineServiceClient
}

func NewVirtualMachineProxy(backend computev1.VirtualMachineServiceClient) *VirtualMachineProxy {
	return &VirtualMachineProxy{backend: backend}
}

func (p *VirtualMachineProxy) Create(ctx context.Context, req *computev1.CreateVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	return p.backend.Create(ctx, req)
}

func (p *VirtualMachineProxy) Get(ctx context.Context, req *computev1.GetVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	return p.backend.Get(ctx, req)
}

func (p *VirtualMachineProxy) List(ctx context.Context, req *computev1.ListVirtualMachinesRequest) (*computev1.ListVirtualMachinesResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *VirtualMachineProxy) Update(ctx context.Context, req *computev1.UpdateVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	return p.backend.Update(ctx, req)
}

func (p *VirtualMachineProxy) Delete(ctx context.Context, req *computev1.DeleteVirtualMachineRequest) (*emptypb.Empty, error) {
	return p.backend.Delete(ctx, req)
}

func (p *VirtualMachineProxy) Watch(req *computev1.WatchVirtualMachinesRequest, stream computev1.VirtualMachineService_WatchServer) error {
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

func (p *VirtualMachineProxy) StreamConsole(req *computev1.StreamConsoleRequest, stream computev1.VirtualMachineService_StreamConsoleServer) error {
	backendStream, err := p.backend.StreamConsole(stream.Context(), req)
	if err != nil {
		return err
	}
	for {
		chunk, err := backendStream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
}
