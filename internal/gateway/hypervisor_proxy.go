package gateway

import (
	"context"
	"io"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

// HypervisorProxy forwards Get/List/Watch to a real compute backend,
// unchanged. Register is deliberately not implemented here (falls through
// to UnimplementedHypervisorServiceServer's default): compute-agent
// self-registers by dialing compute directly, bypassing api-gateway
// entirely (east-west, not north-south -- see docs/architecture.md
// "Hypervisor自己登録とzone割当"), so this proxy is never asked to forward it
// in practice.
//
// GetHypervisorRequest/ListHypervisorsRequest/WatchHypervisorsRequest carry
// no tenant_id (Hypervisor isn't a tenant-scoped resource), which makes
// these admin-only through api-gateway via internal/authz's rule for
// requests without one -- regular KaaS tenants have no legitimate reason to
// see the shared physical Hypervisor inventory.
type HypervisorProxy struct {
	computev1.UnimplementedHypervisorServiceServer
	backend computev1.HypervisorServiceClient
}

func NewHypervisorProxy(backend computev1.HypervisorServiceClient) *HypervisorProxy {
	return &HypervisorProxy{backend: backend}
}

func (p *HypervisorProxy) Get(ctx context.Context, req *computev1.GetHypervisorRequest) (*computev1.Hypervisor, error) {
	return p.backend.Get(ctx, req)
}

func (p *HypervisorProxy) List(ctx context.Context, req *computev1.ListHypervisorsRequest) (*computev1.ListHypervisorsResponse, error) {
	return p.backend.List(ctx, req)
}

func (p *HypervisorProxy) Watch(req *computev1.WatchHypervisorsRequest, stream computev1.HypervisorService_WatchServer) error {
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
