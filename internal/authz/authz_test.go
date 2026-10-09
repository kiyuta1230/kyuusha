package authz

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiyuta1230/kyuusha/internal/authn"
)

type fakeReq struct{ tenantID string }

func (r fakeReq) GetTenantId() string { return r.tenantID }

// fakeUnscopedReq deliberately does not implement TenantIDGetter, mirroring
// identity's CreateTenantRequest: creating a Tenant isn't scoped under an
// existing tenant_id, so it must be admin-only.
type fakeUnscopedReq struct{}

// vmGetMethod/vmCreateMethod/storageconnCreateMethod are representative
// fullMethod values (real ones from the generated services) used wherever a
// test needs some concrete RPC classified as read/write, or as belonging to
// the blockstorage service area -- see rpcclass.go.
const (
	vmGetMethod             = "/kyuusha.compute.v1.VirtualMachineService/Get"
	vmCreateMethod          = "/kyuusha.compute.v1.VirtualMachineService/Create"
	storageconnCreateMethod = "/kyuusha.blockstorage.v1.StorageConnectionService/Create"
	volumeGetMethod         = "/kyuusha.blockstorage.v1.VolumeService/Get"
	subnetCreateMethod      = "/kyuusha.network.v1.SubnetService/Create"
	subnetGetMethod         = "/kyuusha.network.v1.SubnetService/Get"
)

func TestAuthorize(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name       string
		claims     authn.Claims
		reqTenant  string
		wantDenied bool
	}{
		{"same tenant allowed", authn.Claims{TenantID: "tenant-a"}, "tenant-a", false},
		{"different tenant denied", authn.Claims{TenantID: "tenant-a"}, "tenant-b", true},
		{"admin can act on any tenant", authn.Claims{TenantID: "tenant-a", Role: "admin"}, "tenant-b", false},
		{"empty claim tenant denied even if request matches", authn.Claims{TenantID: ""}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := authn.NewContextForTest(ctx, &tt.claims)
			_, _, err := a.authorize(ctx, fakeReq{tenantID: tt.reqTenant}, vmCreateMethod)
			denied := status.Code(err) == codes.PermissionDenied
			if denied != tt.wantDenied {
				t.Fatalf("authorize() err=%v, denied=%v, want denied=%v", err, denied, tt.wantDenied)
			}
		})
	}
}

func TestAuthorize_UnscopedRequestIsAdminOnly(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	nonAdmin := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "tenant-a"})
	if _, _, err := a.authorize(nonAdmin, fakeUnscopedReq{}, vmCreateMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin on unscoped request: got %v, want PermissionDenied", err)
	}

	admin := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "tenant-a", Role: "admin"})
	if _, _, err := a.authorize(admin, fakeUnscopedReq{}, vmCreateMethod); err != nil {
		t.Fatalf("admin on unscoped request: got %v, want allowed", err)
	}
}

// TestAuthorize_StorageAdmin exercises the storage-admin role
// (docs/specs/authn-authz.md "将来の拡張"): cross-tenant power, but only for
// block-storage RPCs -- unlike admin, it must not reach into compute.
func TestAuthorize_StorageAdmin(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	storageAdmin := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "tenant-a", Role: "storage-admin"})

	// An unscoped request (e.g. CreateStorageConnectionRequest, which has no
	// tenant_id at all) for a blockstorage RPC must be allowed.
	if _, _, err := a.authorize(storageAdmin, fakeUnscopedReq{}, storageconnCreateMethod); err != nil {
		t.Fatalf("storage-admin on unscoped blockstorage request: got %v, want allowed", err)
	}
	// A tenant-scoped blockstorage request for a DIFFERENT tenant must also
	// be allowed -- storage-admin is cross-tenant, like admin.
	if _, _, err := a.authorize(storageAdmin, fakeReq{tenantID: "tenant-b"}, volumeGetMethod); err != nil {
		t.Fatalf("storage-admin on another tenant's blockstorage request: got %v, want allowed", err)
	}
	// A non-blockstorage RPC (compute) must be denied even for another
	// tenant's own tenant -- storage-admin has no general cross-tenant power.
	if _, _, err := a.authorize(storageAdmin, fakeReq{tenantID: "tenant-b"}, vmCreateMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("storage-admin on a compute request for another tenant: got %v, want PermissionDenied", err)
	}
	// A non-blockstorage RPC within the storage-admin's own tenant is
	// allowed too -- but via the ordinary ("member") tenant-scoped rule
	// (claims.tenant_id == request.tenant_id), not because of the
	// storage-admin role itself.
	if _, _, err := a.authorize(storageAdmin, fakeReq{tenantID: "tenant-a"}, vmGetMethod); err != nil {
		t.Fatalf("storage-admin's own tenant, compute request: got %v, want allowed (via the ordinary tenant rule)", err)
	}
}

// TestAuthorize_Viewer exercises the viewer tenant_role
// (docs/specs/authn-authz.md "将来の拡張"「軸1」): read-only within the
// caller's own tenant, still denied entirely outside it.
func TestAuthorize_Viewer(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	viewer := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "tenant-a", TenantRole: "viewer"})

	if _, _, err := a.authorize(viewer, fakeReq{tenantID: "tenant-a"}, vmGetMethod); err != nil {
		t.Fatalf("viewer Get within own tenant: got %v, want allowed", err)
	}
	if _, _, err := a.authorize(viewer, fakeReq{tenantID: "tenant-a"}, vmCreateMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer Create within own tenant: got %v, want PermissionDenied", err)
	}
	if _, _, err := a.authorize(viewer, fakeReq{tenantID: "tenant-b"}, vmGetMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer Get on another tenant: got %v, want PermissionDenied", err)
	}
}

// TestAuthorize_NetworkAdmin mirrors TestAuthorize_StorageAdmin exactly,
// swapping in network's own RPCs -- see docs/specs/authn-authz.md
// "将来の拡張".
func TestAuthorize_NetworkAdmin(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	networkAdmin := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "tenant-a", Role: "network-admin"})

	// An unscoped request for a network RPC must be allowed.
	if _, _, err := a.authorize(networkAdmin, fakeUnscopedReq{}, subnetCreateMethod); err != nil {
		t.Fatalf("network-admin on unscoped network request: got %v, want allowed", err)
	}
	// A tenant-scoped network request for a DIFFERENT tenant must also be
	// allowed -- network-admin is cross-tenant, like admin.
	if _, _, err := a.authorize(networkAdmin, fakeReq{tenantID: "tenant-b"}, subnetGetMethod); err != nil {
		t.Fatalf("network-admin on another tenant's network request: got %v, want allowed", err)
	}
	// A non-network RPC (compute) must be denied even for another tenant --
	// network-admin has no general cross-tenant power.
	if _, _, err := a.authorize(networkAdmin, fakeReq{tenantID: "tenant-b"}, vmCreateMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("network-admin on a compute request for another tenant: got %v, want PermissionDenied", err)
	}
	// A non-network RPC within network-admin's own tenant is allowed too --
	// but via the ordinary ("member") tenant-scoped rule, not the
	// network-admin role itself.
	if _, _, err := a.authorize(networkAdmin, fakeReq{tenantID: "tenant-a"}, vmGetMethod); err != nil {
		t.Fatalf("network-admin's own tenant, compute request: got %v, want allowed (via the ordinary tenant rule)", err)
	}
}

// TestAuthorize_GlobalViewer exercises the cross-tenant, cross-service
// viewer role (docs/specs/authn-authz.md "将来の拡張") -- distinct from
// TestAuthorize_Viewer's tenant_role=="viewer", which is read-only within
// one tenant only.
func TestAuthorize_GlobalViewer(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	viewer := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "tenant-a", Role: "viewer"})

	// Read RPCs are allowed across every tenant, including ones the caller
	// doesn't belong to -- this is the whole point of the role.
	if _, _, err := a.authorize(viewer, fakeReq{tenantID: "tenant-b"}, vmGetMethod); err != nil {
		t.Fatalf("global viewer Get on another tenant: got %v, want allowed", err)
	}
	// Unscoped read requests (e.g. HypervisorService.List, TenantService.List
	// -- normally admin-only) are allowed too, since role=="viewer" doesn't
	// depend on request.tenant_id at all.
	if _, _, err := a.authorize(viewer, fakeUnscopedReq{}, vmGetMethod); err != nil {
		t.Fatalf("global viewer on unscoped read request: got %v, want allowed", err)
	}
	// Write RPCs are denied everywhere, including the caller's own tenant --
	// role=="viewer" grants no write power at all.
	if _, _, err := a.authorize(viewer, fakeReq{tenantID: "tenant-a"}, vmCreateMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("global viewer Create in own tenant: got %v, want PermissionDenied", err)
	}
	if _, _, err := a.authorize(viewer, fakeUnscopedReq{}, vmCreateMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("global viewer on unscoped write request: got %v, want PermissionDenied", err)
	}
}

// TestAuthorize_ServiceScopedAdmin covers the <service>-admin naming
// convention, e.g. for an external backend's service registered on
// api-gateway: cross-tenant within that service, nothing outside it.
func TestAuthorize_ServiceScopedAdmin(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	vpcAdmin := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "ops", Role: "vpc-admin"})
	const vpcCreate = "/kyuusha.vpc.v1.VPCService/Create"
	if _, _, err := a.authorize(vpcAdmin, fakeReq{tenantID: "tenant-b"}, vpcCreate); err != nil {
		t.Fatalf("vpc-admin on another tenant's vpc request: got %v, want allowed", err)
	}
	if _, _, err := a.authorize(vpcAdmin, fakeUnscopedReq{}, vpcCreate); err != nil {
		t.Fatalf("vpc-admin on an unscoped vpc request: got %v, want allowed", err)
	}
	if _, _, err := a.authorize(vpcAdmin, fakeReq{tenantID: "tenant-b"}, subnetCreateMethod); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("vpc-admin on a network request: got %v, want PermissionDenied", err)
	}
	// A role can't match a malformed method with no service segment.
	bare := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "ops", Role: "-admin"})
	if _, _, err := a.authorize(bare, fakeUnscopedReq{}, "bogus"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("role -admin on a method with no service: got %v, want PermissionDenied", err)
	}
}

// TestAuthorize_MultipleRoles: roles add up. A controller holding
// network-admin and viewer writes network resources of any tenant and
// reads everything else, but writes nothing outside network -- not even
// in its own nominal tenant.
func TestAuthorize_MultipleRoles(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctrl := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "ops", Roles: authn.StringList{"network-admin", "viewer"}})
	for _, c := range []struct {
		name   string
		tenant string
		method string
		allow  bool
	}{
		{"network write, another tenant", "tenant-b", subnetCreateMethod, true},
		{"compute read, another tenant", "tenant-b", vmGetMethod, true},
		{"block-storage read, another tenant", "tenant-b", volumeGetMethod, true},
		{"compute write, another tenant", "tenant-b", vmCreateMethod, false},
		{"compute write, its own nominal tenant", "ops", vmCreateMethod, false},
	} {
		_, _, err := a.authorize(ctrl, fakeReq{tenantID: c.tenant}, c.method)
		if c.allow && err != nil || !c.allow && status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: got %v, want allow=%v", c.name, err, c.allow)
		}
	}
	// role and roles together count as one set.
	both := authn.NewContextForTest(ctx, &authn.Claims{TenantID: "ops", Role: "storage-admin", Roles: authn.StringList{"network-admin"}})
	for _, m := range []string{storageconnCreateMethod, subnetCreateMethod} {
		if _, _, err := a.authorize(both, fakeReq{tenantID: "tenant-b"}, m); err != nil {
			t.Errorf("role+roles on %s: got %v, want allowed", m, err)
		}
	}
}
