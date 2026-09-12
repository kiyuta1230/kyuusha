// Package grpcserver adapts internal/compute.Service to the generated
// VirtualMachineServiceServer interface. It only translates between wire
// types and domain types; all behavior lives in compute.Service.
package grpcserver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/resource"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

type Server struct {
	computev1.UnimplementedVirtualMachineServiceServer
	svc *compute.Service
	// console is only used by StreamConsole: the NATS request/relay it
	// needs already lives on Reconciler (which owns the NATS connection),
	// so this just reuses it rather than duplicating that plumbing here.
	console *compute.Reconciler
}

func New(svc *compute.Service, console *compute.Reconciler) *Server {
	return &Server{svc: svc, console: console}
}

func (s *Server) Create(ctx context.Context, req *computev1.CreateVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	vm, err := s.svc.Create(ctx, req.GetTenantId(), req.GetName(), fromSpec(req.GetSpec()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toVM(*vm), nil
}

func (s *Server) Get(ctx context.Context, req *computev1.GetVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	vm, err := s.svc.Get(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toVM(*vm), nil
}

func (s *Server) List(ctx context.Context, req *computev1.ListVirtualMachinesRequest) (*computev1.ListVirtualMachinesResponse, error) {
	vms, err := s.svc.List(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &computev1.ListVirtualMachinesResponse{}
	for _, vm := range vms {
		out.Items = append(out.Items, toVM(vm))
	}
	return out, nil
}

func (s *Server) Update(ctx context.Context, req *computev1.UpdateVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	// req.TenantId (top-level) is what internal/authz actually authorized;
	// req.Vm.Meta.TenantId must agree, or a caller authorized for their own
	// tenant could smuggle a mutation of another tenant's VM inside vm.meta.
	if req.GetTenantId() == "" || req.GetTenantId() != req.GetVm().GetMeta().GetTenantId() {
		return nil, status.Error(codes.InvalidArgument, "tenant_id must be set and match vm.meta.tenant_id")
	}
	vm := fromVM(req.GetVm())
	updated, err := s.svc.Update(ctx, &vm)
	if err != nil {
		return nil, toStatus(err)
	}
	return toVM(*updated), nil
}

func (s *Server) Delete(ctx context.Context, req *computev1.DeleteVirtualMachineRequest) (*emptypb.Empty, error) {
	if err := s.svc.Delete(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) Stop(ctx context.Context, req *computev1.StopVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	vm, err := s.svc.Stop(ctx, req.GetTenantId(), req.GetId(), req.GetForce())
	if err != nil {
		return nil, toStatus(err)
	}
	return toVM(*vm), nil
}

func (s *Server) Start(ctx context.Context, req *computev1.StartVirtualMachineRequest) (*computev1.VirtualMachine, error) {
	vm, err := s.svc.Start(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toVM(*vm), nil
}

func (s *Server) Watch(req *computev1.WatchVirtualMachinesRequest, stream computev1.VirtualMachineService_WatchServer) error {
	events, err := s.svc.Watch(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion(), req.GetFinalizerName())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		if err := stream.Send(toEvent(e)); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func (s *Server) StreamConsole(req *computev1.StreamConsoleRequest, stream computev1.VirtualMachineService_StreamConsoleServer) error {
	if req.GetTenantId() == "" {
		return status.Error(codes.InvalidArgument, "tenant_id is required")
	}
	chunks, err := s.console.StreamConsole(stream.Context(), req.GetTenantId(), req.GetId(), req.GetTailBytes(), req.GetFollow())
	if err != nil {
		return toStatus(err)
	}
	for data := range chunks {
		if err := stream.Send(&computev1.ConsoleChunk{Data: data}); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, compute.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, compute.ErrConflict):
		return status.Error(codes.Aborted, err.Error())
	case errors.Is(err, compute.ErrHistoryPruned):
		return status.Error(codes.OutOfRange, err.Error())
	case errors.Is(err, compute.ErrQuotaExceeded):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, compute.ErrInvalidPhase):
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

func fromSpec(s *computev1.VirtualMachineSpec) compute.VirtualMachineSpec {
	spec := compute.VirtualMachineSpec{
		ImageID:    s.GetImageId(),
		VCPU:       s.GetVcpu(),
		MemoryMB:   s.GetMemoryMb(),
		UserData:   s.GetUserData(),
		DriverHint: fromDriver(s.GetDriverHint()),
	}
	for _, n := range s.GetNetworkInterfaces() {
		spec.NetworkInterfaces = append(spec.NetworkInterfaces, compute.NetworkAttachment{
			SubnetID: n.GetSubnetId(),
			Primary:  n.GetPrimary(),
		})
	}
	for _, v := range s.GetVolumes() {
		spec.Volumes = append(spec.Volumes, compute.VolumeRequest{
			VolumeID:   v.GetVolumeId(),
			DeviceHint: v.GetDeviceHint(),
		})
	}
	for _, p := range s.GetPciDevices() {
		spec.PciDevices = append(spec.PciDevices, compute.PciDeviceRequest{
			VendorID: p.GetVendorId(),
			DeviceID: p.GetDeviceId(),
			Count:    p.GetCount(),
		})
	}
	return spec
}

func toSpec(s compute.VirtualMachineSpec) *computev1.VirtualMachineSpec {
	out := &computev1.VirtualMachineSpec{
		ImageId:    s.ImageID,
		Vcpu:       s.VCPU,
		MemoryMb:   s.MemoryMB,
		UserData:   s.UserData,
		DriverHint: toDriver(s.DriverHint),
	}
	for _, n := range s.NetworkInterfaces {
		out.NetworkInterfaces = append(out.NetworkInterfaces, &computev1.NetworkAttachment{
			SubnetId: n.SubnetID,
			Primary:  n.Primary,
		})
	}
	for _, v := range s.Volumes {
		out.Volumes = append(out.Volumes, &computev1.VolumeRequest{
			VolumeId:   v.VolumeID,
			DeviceHint: v.DeviceHint,
		})
	}
	for _, p := range s.PciDevices {
		out.PciDevices = append(out.PciDevices, &computev1.PciDeviceRequest{
			VendorId: p.VendorID,
			DeviceId: p.DeviceID,
			Count:    p.Count,
		})
	}
	return out
}

func toStatusProto(st compute.VirtualMachineStatus) *computev1.VirtualMachineStatus {
	out := &computev1.VirtualMachineStatus{
		Phase:                string(st.Phase),
		Hypervisor:           st.Hypervisor,
		InterfaceRefs:        st.InterfaceRefs,
		VolumeAttachmentRefs: st.VolumeAttachmentRefs,
	}
	for _, c := range st.Conditions {
		out.Conditions = append(out.Conditions, &resourcev1.Condition{
			Type:             c.Type,
			Status:           string(c.Status),
			Reason:           c.Reason,
			Message:          c.Message,
			LastTransitionAt: timestamppb.New(c.LastTransitionAt),
		})
	}
	return out
}

func fromStatusProto(st *computev1.VirtualMachineStatus) compute.VirtualMachineStatus {
	out := compute.VirtualMachineStatus{
		Phase:                compute.Phase(st.GetPhase()),
		Hypervisor:           st.GetHypervisor(),
		InterfaceRefs:        st.GetInterfaceRefs(),
		VolumeAttachmentRefs: st.GetVolumeAttachmentRefs(),
	}
	for _, c := range st.GetConditions() {
		out.Conditions = append(out.Conditions, resource.Condition{
			Type:             c.GetType(),
			Status:           resource.ConditionStatus(c.GetStatus()),
			Reason:           c.GetReason(),
			Message:          c.GetMessage(),
			LastTransitionAt: c.GetLastTransitionAt().AsTime(),
		})
	}
	return out
}

func toFinalizersProto(fs []resource.Finalizer) []*resourcev1.Finalizer {
	out := make([]*resourcev1.Finalizer, len(fs))
	for i, f := range fs {
		out[i] = &resourcev1.Finalizer{Name: f.Name, AddedBy: f.AddedBy}
	}
	return out
}

func fromFinalizersProto(fs []*resourcev1.Finalizer) []resource.Finalizer {
	out := make([]resource.Finalizer, len(fs))
	for i, f := range fs {
		out[i] = resource.Finalizer{Name: f.GetName(), AddedBy: f.GetAddedBy()}
	}
	return out
}

func toVM(vm compute.VirtualMachine) *computev1.VirtualMachine {
	meta := &resourcev1.ObjectMeta{
		Id:              vm.Meta.ID,
		Name:            vm.Meta.Name,
		TenantId:        vm.Meta.TenantID,
		ResourceVersion: vm.Meta.ResourceVersion,
		CreatedAt:       timestamppb.New(vm.Meta.CreatedAt),
		Finalizers:      toFinalizersProto(vm.Meta.Finalizers),
	}
	if vm.Meta.DeletedAt != nil {
		meta.DeletedAt = timestamppb.New(*vm.Meta.DeletedAt)
	}
	return &computev1.VirtualMachine{
		Meta:   meta,
		Spec:   toSpec(vm.Spec),
		Status: toStatusProto(vm.Status),
	}
}

func fromVM(vm *computev1.VirtualMachine) compute.VirtualMachine {
	meta := vm.GetMeta()
	out := compute.VirtualMachine{
		Meta: resource.ObjectMeta{
			ID:              meta.GetId(),
			Name:            meta.GetName(),
			TenantID:        meta.GetTenantId(),
			ResourceVersion: meta.GetResourceVersion(),
			CreatedAt:       meta.GetCreatedAt().AsTime(),
			Finalizers:      fromFinalizersProto(meta.GetFinalizers()),
		},
		Spec:   fromSpec(vm.GetSpec()),
		Status: fromStatusProto(vm.GetStatus()),
	}
	if meta.GetDeletedAt() != nil {
		t := meta.GetDeletedAt().AsTime()
		out.Meta.DeletedAt = &t
	}
	return out
}

func toEvent(e compute.Event) *computev1.VirtualMachineEvent {
	out := &computev1.VirtualMachineEvent{ResourceVersion: e.ResourceVersion}
	switch e.Type {
	case compute.EventAdded:
		out.Type = computev1.VirtualMachineEvent_ADDED
	case compute.EventModified:
		out.Type = computev1.VirtualMachineEvent_MODIFIED
	case compute.EventDeleted:
		out.Type = computev1.VirtualMachineEvent_DELETED
	case compute.EventBookmark:
		out.Type = computev1.VirtualMachineEvent_BOOKMARK
	}
	if out.Type != computev1.VirtualMachineEvent_BOOKMARK {
		out.Vm = toVM(e.Object)
	}
	return out
}

func fromDriver(d computev1.VmmDriver) compute.VmmDriver {
	switch d {
	case computev1.VmmDriver_VMM_DRIVER_FIRECRACKER:
		return compute.VmmDriverFirecracker
	case computev1.VmmDriver_VMM_DRIVER_CLOUD_HYPERVISOR:
		return compute.VmmDriverCloudHypervisor
	default:
		return compute.VmmDriverUnspecified
	}
}

func toDriver(d compute.VmmDriver) computev1.VmmDriver {
	switch d {
	case compute.VmmDriverFirecracker:
		return computev1.VmmDriver_VMM_DRIVER_FIRECRACKER
	case compute.VmmDriverCloudHypervisor:
		return computev1.VmmDriver_VMM_DRIVER_CLOUD_HYPERVISOR
	default:
		return computev1.VmmDriver_VMM_DRIVER_UNSPECIFIED
	}
}
