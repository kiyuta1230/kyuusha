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
			err := a.authorize(ctx, fakeReq{tenantID: tt.reqTenant})
			denied := status.Code(err) == codes.PermissionDenied
			if denied != tt.wantDenied {
				t.Fatalf("authorize() err=%v, denied=%v, want denied=%v", err, denied, tt.wantDenied)
			}
		})
	}
}
