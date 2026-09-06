package compute

import (
	"context"
	"math"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/image/v1"
	networkv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/network/v1"
	resourcev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

// UnlimitedQuota is a QuotaSpec generous enough that quota enforcement never
// interferes with a test that isn't specifically exercising it.
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
// Tenant carrying Quota (default UnlimitedQuota() if left nil); every other
// method panics since compute.Service never calls them.
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
	panic("FakeTenantClient: Create not implemented; compute.Service never calls it")
}

func (f *FakeTenantClient) List(context.Context, *identityv1.ListTenantsRequest, ...grpc.CallOption) (*identityv1.ListTenantsResponse, error) {
	panic("FakeTenantClient: List not implemented; compute.Service never calls it")
}

func (f *FakeTenantClient) Update(context.Context, *identityv1.UpdateTenantRequest, ...grpc.CallOption) (*identityv1.Tenant, error) {
	panic("FakeTenantClient: Update not implemented; compute.Service never calls it")
}

func (f *FakeTenantClient) Delete(context.Context, *identityv1.DeleteTenantRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	panic("FakeTenantClient: Delete not implemented; compute.Service never calls it")
}

func (f *FakeTenantClient) Watch(context.Context, *identityv1.WatchTenantsRequest, ...grpc.CallOption) (identityv1.TenantService_WatchClient, error) {
	panic("FakeTenantClient: Watch not implemented; compute.Service never calls it")
}

// FakeImageClient is a minimal imagev1.ImageServiceClient for tests that
// don't want to run a real image server: Get always returns a Ready Image
// in the given Format (default KERNEL_ROOTFS, matching the default
// driver_hint FIRECRACKER) regardless of the requested id, unless the id is
// empty; every other method panics since compute.Service never calls them.
type FakeImageClient struct {
	Format imagev1.ImageFormat // default: KERNEL_ROOTFS
	Phase  string              // default: "Ready"
}

func (f *FakeImageClient) format() imagev1.ImageFormat {
	if f.Format != imagev1.ImageFormat_IMAGE_FORMAT_UNSPECIFIED {
		return f.Format
	}
	return imagev1.ImageFormat_KERNEL_ROOTFS
}

func (f *FakeImageClient) phase() string {
	if f.Phase != "" {
		return f.Phase
	}
	return "Ready"
}

func (f *FakeImageClient) Get(ctx context.Context, req *imagev1.GetImageRequest, opts ...grpc.CallOption) (*imagev1.Image, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.NotFound, "image: not found")
	}
	return &imagev1.Image{
		Meta:   &resourcev1.ObjectMeta{Id: req.GetId(), TenantId: req.GetTenantId()},
		Spec:   &imagev1.ImageSpec{Format: f.format()},
		Status: &imagev1.ImageStatus{Phase: f.phase()},
	}, nil
}

func (f *FakeImageClient) Create(context.Context, *imagev1.CreateImageRequest, ...grpc.CallOption) (*imagev1.Image, error) {
	panic("FakeImageClient: Create not implemented; compute.Service never calls it")
}

func (f *FakeImageClient) List(context.Context, *imagev1.ListImagesRequest, ...grpc.CallOption) (*imagev1.ListImagesResponse, error) {
	panic("FakeImageClient: List not implemented; compute.Service never calls it")
}

func (f *FakeImageClient) Delete(context.Context, *imagev1.DeleteImageRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	panic("FakeImageClient: Delete not implemented; compute.Service never calls it")
}

func (f *FakeImageClient) Watch(context.Context, *imagev1.WatchImagesRequest, ...grpc.CallOption) (imagev1.ImageService_WatchClient, error) {
	panic("FakeImageClient: Watch not implemented; compute.Service never calls it")
}

// FakeSubnetClient is a minimal networkv1.SubnetServiceClient for tests
// that don't want to run a real network server: Get always returns a Ready
// Subnet in the given Zone (default "zone-a") with a plausible CIDR/
// gateway_ip/vlan_id (so createNetworkInterfaces has something real to
// build a NetworkInterfaceInfo from) regardless of the requested id, unless
// the id is empty; every other method panics since compute.Service never
// calls them.
type FakeSubnetClient struct {
	Zone  string // default: "zone-a"
	Phase string // default: "Ready"
}

func (f *FakeSubnetClient) zone() string {
	if f.Zone != "" {
		return f.Zone
	}
	return "zone-a"
}

func (f *FakeSubnetClient) phase() string {
	if f.Phase != "" {
		return f.Phase
	}
	return "Ready"
}

func (f *FakeSubnetClient) Get(ctx context.Context, req *networkv1.GetSubnetRequest, opts ...grpc.CallOption) (*networkv1.Subnet, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.NotFound, "subnet: not found")
	}
	return &networkv1.Subnet{
		Meta: &resourcev1.ObjectMeta{Id: req.GetId(), TenantId: req.GetTenantId()},
		Spec: &networkv1.SubnetSpec{
			Zone:      f.zone(),
			Cidr:      "10.0.0.0/24",
			GatewayIp: "10.0.0.1",
		},
		Status: &networkv1.SubnetStatus{Phase: f.phase(), VlanId: 1},
	}, nil
}

func (f *FakeSubnetClient) Create(context.Context, *networkv1.CreateSubnetRequest, ...grpc.CallOption) (*networkv1.Subnet, error) {
	panic("FakeSubnetClient: Create not implemented; compute.Service never calls it")
}

func (f *FakeSubnetClient) List(context.Context, *networkv1.ListSubnetsRequest, ...grpc.CallOption) (*networkv1.ListSubnetsResponse, error) {
	panic("FakeSubnetClient: List not implemented; compute.Service never calls it")
}

func (f *FakeSubnetClient) Update(context.Context, *networkv1.UpdateSubnetRequest, ...grpc.CallOption) (*networkv1.Subnet, error) {
	panic("FakeSubnetClient: Update not implemented; compute.Service never calls it")
}

func (f *FakeSubnetClient) Delete(context.Context, *networkv1.DeleteSubnetRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	panic("FakeSubnetClient: Delete not implemented; compute.Service never calls it")
}

func (f *FakeSubnetClient) Watch(context.Context, *networkv1.WatchSubnetsRequest, ...grpc.CallOption) (networkv1.SubnetService_WatchClient, error) {
	panic("FakeSubnetClient: Watch not implemented; compute.Service never calls it")
}

// FakeNetworkInterfaceClient is a minimal
// networkv1.NetworkInterfaceServiceClient for tests: Create always
// succeeds, deriving the id from the request's name (deterministic and
// traceable in test assertions, unlike network's own random IDs) and
// allocating a plausible IP/MAC (so createNetworkInterfaces' full
// NetworkInterfaceInfo path is exercised, not just the Pending/no-IP
// short-circuit); every other method panics since compute.Service never
// calls them.
type FakeNetworkInterfaceClient struct {
	// Pending, if true, simulates a Subnet whose IP pool is exhausted (see
	// docs/specs/network.md): Create still succeeds, but the returned
	// NetworkInterface has no IP/MAC allocated yet.
	Pending bool
}

func (f *FakeNetworkInterfaceClient) Create(ctx context.Context, req *networkv1.CreateNetworkInterfaceRequest, opts ...grpc.CallOption) (*networkv1.NetworkInterface, error) {
	if f.Pending {
		return &networkv1.NetworkInterface{
			Meta:   &resourcev1.ObjectMeta{Id: "netif-" + req.GetName(), TenantId: req.GetTenantId()},
			Spec:   req.GetSpec(),
			Status: &networkv1.NetworkInterfaceStatus{Phase: "Pending"},
		}, nil
	}
	return &networkv1.NetworkInterface{
		Meta: &resourcev1.ObjectMeta{Id: "netif-" + req.GetName(), TenantId: req.GetTenantId()},
		Spec: req.GetSpec(),
		Status: &networkv1.NetworkInterfaceStatus{
			Phase:      "Ready",
			IpAddress:  "10.0.0.5",
			MacAddress: "02:00:00:00:00:01",
		},
	}, nil
}

func (f *FakeNetworkInterfaceClient) Get(context.Context, *networkv1.GetNetworkInterfaceRequest, ...grpc.CallOption) (*networkv1.NetworkInterface, error) {
	panic("FakeNetworkInterfaceClient: Get not implemented; compute.Service never calls it")
}

func (f *FakeNetworkInterfaceClient) List(context.Context, *networkv1.ListNetworkInterfacesRequest, ...grpc.CallOption) (*networkv1.ListNetworkInterfacesResponse, error) {
	panic("FakeNetworkInterfaceClient: List not implemented; compute.Service never calls it")
}

func (f *FakeNetworkInterfaceClient) Update(context.Context, *networkv1.UpdateNetworkInterfaceRequest, ...grpc.CallOption) (*networkv1.NetworkInterface, error) {
	panic("FakeNetworkInterfaceClient: Update not implemented; compute.Service never calls it")
}

func (f *FakeNetworkInterfaceClient) Delete(context.Context, *networkv1.DeleteNetworkInterfaceRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	panic("FakeNetworkInterfaceClient: Delete not implemented; compute.Service never calls it")
}

func (f *FakeNetworkInterfaceClient) Watch(context.Context, *networkv1.WatchNetworkInterfacesRequest, ...grpc.CallOption) (networkv1.NetworkInterfaceService_WatchClient, error) {
	panic("FakeNetworkInterfaceClient: Watch not implemented; compute.Service never calls it")
}
