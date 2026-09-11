package authz

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gitlab.com/ki.yuta1230/kyuusha/internal/authn"
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
