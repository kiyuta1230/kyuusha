package grpcserver

import (
	"context"
	"crypto"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kiyuta1230/kyuusha/internal/bootstraptoken"
	"github.com/kiyuta1230/kyuusha/internal/compute"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

// HypervisorServer implements HypervisorServiceServer. It's a separate type
// from Server (rather than one type implementing both generated interfaces)
// because VirtualMachineServiceServer and HypervisorServiceServer both
// declare Get/List/Watch methods with different signatures -- Go doesn't
// allow one type to satisfy both. Both wrap the same compute.Service and are
// registered on the same grpc.Server (see cmd/compute/main.go).
type HypervisorServer struct {
	computev1.UnimplementedHypervisorServiceServer
	svc                     *compute.Service
	bootstrapTokenPublicKey crypto.PublicKey
}

func NewHypervisorServer(svc *compute.Service, bootstrapTokenPublicKey crypto.PublicKey) *HypervisorServer {
	return &HypervisorServer{svc: svc, bootstrapTokenPublicKey: bootstrapTokenPublicKey}
}

// Register verifies the caller's zone-scoped bootstrap token
// (internal/bootstraptoken, docs/architecture.md "Hypervisor自己登録とzone割当")
// and registers the Hypervisor into the token's zone -- never a
// self-reported one, since the agent's own claim isn't trusted. If the
// token also carries a hypervisor_id claim, it must match req.Hypervisor
// exactly -- a token minted for one hypervisor can't be replayed to
// register a different one under its name.
func (s *HypervisorServer) Register(ctx context.Context, req *computev1.RegisterHypervisorRequest) (*computev1.Hypervisor, error) {
	if req.GetHypervisor() == "" {
		return nil, status.Error(codes.InvalidArgument, "hypervisor is required")
	}
	if req.GetBootstrapToken() == "" {
		return nil, status.Error(codes.Unauthenticated, "bootstrap_token is required")
	}
	claims, err := bootstraptoken.VerifyWithKey(s.bootstrapTokenPublicKey, req.GetBootstrapToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid bootstrap_token: %v", err)
	}
	if claims.HypervisorID != "" && claims.HypervisorID != req.GetHypervisor() {
		return nil, status.Errorf(codes.PermissionDenied, "bootstrap_token is scoped to hypervisor %q, not %q", claims.HypervisorID, req.GetHypervisor())
	}
	h, err := s.svc.RegisterHypervisor(ctx, req.GetHypervisor(), claims.Zone, req.GetAllocatableVcpu(), req.GetAllocatableMemoryMb(), req.GetSupportedDrivers(), fromStorageConnectionsProto(req.GetStorageConnections()), fromPciDevicesProto(req.GetAvailableDevices()), fromNumaNodesProto(req.GetNumaNodes()))
	if err != nil {
		return nil, toHypervisorStatus(err)
	}
	return toHypervisor(*h), nil
}

func (s *HypervisorServer) Get(ctx context.Context, req *computev1.GetHypervisorRequest) (*computev1.Hypervisor, error) {
	h, err := s.svc.GetHypervisor(ctx, req.GetHypervisor())
	if err != nil {
		return nil, toHypervisorStatus(err)
	}
	return toHypervisor(*h), nil
}

func (s *HypervisorServer) List(ctx context.Context, req *computev1.ListHypervisorsRequest) (*computev1.ListHypervisorsResponse, error) {
	hs, err := s.svc.ListHypervisors(ctx)
	if err != nil {
		return nil, toHypervisorStatus(err)
	}
	out := &computev1.ListHypervisorsResponse{}
	for _, h := range hs {
		out.Items = append(out.Items, toHypervisor(h))
	}
	return out, nil
}

func (s *HypervisorServer) SetSchedulable(ctx context.Context, req *computev1.SetSchedulableRequest) (*computev1.Hypervisor, error) {
	if req.GetHypervisor() == "" {
		return nil, status.Error(codes.InvalidArgument, "hypervisor is required")
	}
	h, err := s.svc.SetSchedulable(ctx, req.GetHypervisor(), req.GetSchedulable())
	if err != nil {
		return nil, toHypervisorStatus(err)
	}
	return toHypervisor(*h), nil
}

func (s *HypervisorServer) SetRevoked(ctx context.Context, req *computev1.SetRevokedRequest) (*computev1.Hypervisor, error) {
	if req.GetHypervisor() == "" {
		return nil, status.Error(codes.InvalidArgument, "hypervisor is required")
	}
	h, err := s.svc.SetRevoked(ctx, req.GetHypervisor(), req.GetRevoked())
	if err != nil {
		return nil, toHypervisorStatus(err)
	}
	return toHypervisor(*h), nil
}

func (s *HypervisorServer) Watch(req *computev1.WatchHypervisorsRequest, stream computev1.HypervisorService_WatchServer) error {
	events, err := s.svc.WatchHypervisors(stream.Context(), req.GetSinceResourceVersion())
	if err != nil {
		return toHypervisorStatus(err)
	}
	for e := range events {
		if err := stream.Send(toHypervisorEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func toHypervisorStatus(err error) error {
	switch {
	case errors.Is(err, compute.ErrHypervisorNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, compute.ErrHypervisorConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, compute.ErrHypervisorHistoryPruned):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, compute.ErrHypervisorRevoked):
		return status.Error(codes.PermissionDenied, err.Error())
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func toHypervisorStatusProto(st compute.HypervisorStatus) *computev1.HypervisorStatus {
	out := &computev1.HypervisorStatus{
		Phase:               string(st.Phase),
		Zone:                st.Zone,
		LastHeartbeatAt:     st.LastHeartbeatAt.Unix(),
		AllocatableVcpu:     st.AllocatableVCPU,
		AllocatableMemoryMb: st.AllocatableMemoryMB,
		AllocatedVcpu:       st.AllocatedVCPU,
		AllocatedMemoryMb:   st.AllocatedMemoryMB,
		SupportedDrivers:    st.SupportedDrivers,
	}
	for _, d := range st.AvailableDevices {
		out.AvailableDevices = append(out.AvailableDevices, &computev1.PciDevice{
			PciAddress: d.PCIAddress,
			VendorId:   d.VendorID,
			DeviceId:   d.DeviceID,
			Allocated:  d.Allocated,
			NumaNode:   d.NumaNode,
		})
	}
	out.NumaNodes = toNumaNodesProto(st.NumaNodes)
	for _, c := range st.StorageConnections {
		out.StorageConnections = append(out.StorageConnections, &computev1.StorageConnection{
			Name:      c.Name,
			LocalPath: c.LocalPath,
		})
	}
	return out
}

func fromStorageConnectionsProto(in []*computev1.StorageConnection) []compute.StorageConnection {
	if len(in) == 0 {
		return nil
	}
	out := make([]compute.StorageConnection, len(in))
	for i, c := range in {
		out[i] = compute.StorageConnection{Name: c.GetName(), LocalPath: c.GetLocalPath()}
	}
	return out
}

// fromPciDevicesProto ignores each entry's Allocated field: RegisterHypervisor
// derives allocation state itself by matching pci_address against the
// previous registration (see its doc comment), never from what the agent
// self-reports.
func fromPciDevicesProto(in []*computev1.PciDevice) []compute.PciDevice {
	if len(in) == 0 {
		return nil
	}
	out := make([]compute.PciDevice, len(in))
	for i, d := range in {
		out[i] = compute.PciDevice{PCIAddress: d.GetPciAddress(), VendorID: d.GetVendorId(), DeviceID: d.GetDeviceId(), NumaNode: d.GetNumaNode()}
	}
	return out
}

// fromNumaNodesProto ignores each entry's allocated_vcpu/allocated_memory_mb
// on input, the same way fromPciDevicesProto ignores allocated: those are
// server-managed scheduling state RegisterHypervisor derives itself by
// matching NodeID against the previous registration, never from what the
// agent self-reports (see hypervisor.proto's RegisterHypervisorRequest.
// numa_nodes).
func fromNumaNodesProto(in []*computev1.NumaNode) []compute.NumaNode {
	if len(in) == 0 {
		return nil
	}
	out := make([]compute.NumaNode, len(in))
	for i, n := range in {
		out[i] = compute.NumaNode{NodeID: n.GetNodeId(), CPUs: n.GetCpus(), MemoryMB: n.GetMemoryMb()}
	}
	return out
}

func toNumaNodesProto(in []compute.NumaNode) []*computev1.NumaNode {
	if len(in) == 0 {
		return nil
	}
	out := make([]*computev1.NumaNode, len(in))
	for i, n := range in {
		out[i] = &computev1.NumaNode{
			NodeId:            n.NodeID,
			Cpus:              n.CPUs,
			MemoryMb:          n.MemoryMB,
			AllocatedVcpu:     n.AllocatedVCPU,
			AllocatedMemoryMb: n.AllocatedMemoryMB,
		}
	}
	return out
}

func toHypervisor(h compute.Hypervisor) *computev1.Hypervisor {
	meta := &resourcev1.ObjectMeta{
		Id:              h.Meta.ID,
		Name:            h.Meta.Name,
		TenantId:        h.Meta.TenantID,
		ResourceVersion: h.Meta.ResourceVersion,
		CreatedAt:       timestamppb.New(h.Meta.CreatedAt),
		Finalizers:      toFinalizersProto(h.Meta.Finalizers),
	}
	if h.Meta.DeletedAt != nil {
		meta.DeletedAt = timestamppb.New(*h.Meta.DeletedAt)
	}
	return &computev1.Hypervisor{
		Meta:   meta,
		Spec:   &computev1.HypervisorSpec{Schedulable: h.Spec.Schedulable, Revoked: h.Spec.Revoked},
		Status: toHypervisorStatusProto(h.Status),
	}
}

func toHypervisorEvent(e compute.HypervisorEvent) *computev1.HypervisorEvent {
	out := &computev1.HypervisorEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case compute.EventAdded:
		out.Type = computev1.HypervisorEvent_ADDED
	case compute.EventModified:
		out.Type = computev1.HypervisorEvent_MODIFIED
	case compute.EventDeleted:
		out.Type = computev1.HypervisorEvent_DELETED
	case compute.EventBookmark:
		out.Type = computev1.HypervisorEvent_BOOKMARK
	}
	if out.Type != computev1.HypervisorEvent_BOOKMARK {
		out.Hypervisor = toHypervisor(e.Object)
	}
	return out
}
