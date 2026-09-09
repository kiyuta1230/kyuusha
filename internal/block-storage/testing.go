package blockstorage

import (
	"context"
	"math"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	resourcev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/resource/v1"
	storageagentv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/storageagent/v1"
)

// UnlimitedQuota is a QuotaSpec generous enough that quota enforcement never
// interferes with a test that isn't specifically exercising it. Mirrors
// compute's identical helper.
func UnlimitedQuota() *identityv1.QuotaSpec {
	return &identityv1.QuotaSpec{
		MaxVcpu:          math.MaxInt32,
		MaxMemoryMb:      math.MaxInt64,
		MaxVolumeGb:      math.MaxInt64,
		MaxVms:           math.MaxInt32,
		MaxVcpuPerVm:     math.MaxInt32,
		MaxMemoryMbPerVm: math.MaxInt64,
	}
}

// FakeTenantClient is a minimal identityv1.TenantServiceClient for tests
// that don't want to run a real identity server: Get always returns a
// Tenant carrying Quota (default UnlimitedQuota() if left nil) unless the
// requested tenant_id is empty; every other method panics since
// blockstorage.Service never calls them.
type FakeTenantClient struct {
	Quota *identityv1.QuotaSpec
}

func (f *FakeTenantClient) quota() *identityv1.QuotaSpec {
	if f.Quota != nil {
		return f.Quota
	}
	return UnlimitedQuota()
}

func (f *FakeTenantClient) Get(ctx context.Context, req *identityv1.GetTenantRequest, opts ...grpc.CallOption) (*identityv1.Tenant, error) {
	if req.GetTenantId() == "" {
		return nil, status.Error(codes.NotFound, "tenant: not found")
	}
	return &identityv1.Tenant{
		Meta: &resourcev1.ObjectMeta{Id: req.GetTenantId(), TenantId: req.GetTenantId()},
		Spec: &identityv1.TenantSpec{Quota: f.quota()},
	}, nil
}

func (f *FakeTenantClient) Create(context.Context, *identityv1.CreateTenantRequest, ...grpc.CallOption) (*identityv1.Tenant, error) {
	panic("FakeTenantClient: Create not implemented; blockstorage.Service never calls it")
}

func (f *FakeTenantClient) List(context.Context, *identityv1.ListTenantsRequest, ...grpc.CallOption) (*identityv1.ListTenantsResponse, error) {
	panic("FakeTenantClient: List not implemented; blockstorage.Service never calls it")
}

func (f *FakeTenantClient) Update(context.Context, *identityv1.UpdateTenantRequest, ...grpc.CallOption) (*identityv1.Tenant, error) {
	panic("FakeTenantClient: Update not implemented; blockstorage.Service never calls it")
}

func (f *FakeTenantClient) Delete(context.Context, *identityv1.DeleteTenantRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	panic("FakeTenantClient: Delete not implemented; blockstorage.Service never calls it")
}

func (f *FakeTenantClient) Watch(context.Context, *identityv1.WatchTenantsRequest, ...grpc.CallOption) (identityv1.TenantService_WatchClient, error) {
	panic("FakeTenantClient: Watch not implemented; blockstorage.Service never calls it")
}

// FakeStorageAgentClient is a minimal
// storageagentv1.StorageBackendServiceClient for tests that don't want a
// real ZFS/iSCSI storage-agent: every call trivially succeeds (no real
// backend, so nothing to actually create/export), so tests can exercise
// blockstorage.Service's own logic (Quota, exclusive-attach, idempotency)
// without depending on host kernel features. ExportVolume returns a fixed,
// clearly-fake IQN/portal.
type FakeStorageAgentClient struct{}

func (f *FakeStorageAgentClient) CreateVolume(context.Context, *storageagentv1.CreateVolumeRequest, ...grpc.CallOption) (*storageagentv1.CreateVolumeResponse, error) {
	return &storageagentv1.CreateVolumeResponse{}, nil
}

func (f *FakeStorageAgentClient) DeleteVolume(context.Context, *storageagentv1.DeleteVolumeRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (f *FakeStorageAgentClient) ExportVolume(ctx context.Context, req *storageagentv1.ExportVolumeRequest, opts ...grpc.CallOption) (*storageagentv1.ExportVolumeResponse, error) {
	return &storageagentv1.ExportVolumeResponse{
		TargetIqn: "iqn.2026-09.io.kyuusha.fake:" + req.GetVolumeId(),
		Portal:    "fake-storage-agent:3260",
	}, nil
}

func (f *FakeStorageAgentClient) UnexportVolume(context.Context, *storageagentv1.UnexportVolumeRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
