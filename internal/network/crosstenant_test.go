package network

import (
	"context"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestService_EmptyTenantListAndWatchSpanAllTenants pins down the
// cross-tenant List/Watch contract docs/specs/external-integration.md
// documents for external controllers: an empty tenant_id returns every
// tenant's objects, and a Watch resumed from a resource_version keeps
// spanning every tenant.
func TestService_EmptyTenantListAndWatchSpanAllTenants(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	a := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn-a", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-b", "sn-b", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"})

	all, err := svc.ListSubnets(ctx, "")
	if err != nil {
		t.Fatalf("ListSubnets(\"\"): %v", err)
	}
	tenants := map[string]bool{}
	for _, sn := range all {
		tenants[sn.Meta.TenantID] = true
	}
	if !tenants["tenant-a"] || !tenants["tenant-b"] {
		t.Fatalf("ListSubnets(\"\") tenants = %v, want both tenant-a and tenant-b", tenants)
	}

	current, err := svc.GetSubnet(ctx, "tenant-a", a.Meta.ID)
	if err != nil {
		t.Fatalf("GetSubnet: %v", err)
	}
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := svc.WatchSubnets(watchCtx, "", current.Meta.ResourceVersion)
	if err != nil {
		t.Fatalf("WatchSubnets(\"\"): %v", err)
	}
	mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-c", "sn-c", SubnetSpec{Zone: "zone-a", CIDR: "10.0.3.0/24"})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("watch closed before tenant-c's Subnet showed up")
			}
			if e.Object.Meta.TenantID == "tenant-c" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for tenant-c's Subnet on the cross-tenant watch")
		}
	}
}
