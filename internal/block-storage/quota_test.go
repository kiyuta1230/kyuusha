package blockstorage

import (
	"context"
	"errors"
	"testing"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestService_CreateVolumeEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{MaxVolumeGb: 100}}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mustCreateTestStorageConnection(t, ctx, svc, "test-connection", "test-zone")
	const tenant = "tenant-a"

	first, err := svc.CreateVolume(ctx, tenant, "vol-1", testVolumeSpec(60))
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}

	// Tenant total would go to 110 > max_volume_gb=100.
	if _, err := svc.CreateVolume(ctx, tenant, "vol-2", testVolumeSpec(50)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over quota: got %v, want ErrQuotaExceeded", err)
	}

	// A second Volume that fits should still succeed.
	second, err := svc.CreateVolume(ctx, tenant, "vol-3", testVolumeSpec(40))
	if err != nil {
		t.Fatalf("second CreateVolume (within remaining quota): %v", err)
	}

	// Idempotent re-Create of the first Volume must not re-charge quota (a
	// third distinct Volume would otherwise now fit: used=100==max=100, no
	// room; but re-Create of an existing name must succeed regardless).
	again, err := svc.CreateVolume(ctx, tenant, "vol-1", testVolumeSpec(60))
	if err != nil {
		t.Fatalf("idempotent re-Create: %v", err)
	}
	if again.Meta.ID != first.Meta.ID {
		t.Fatalf("idempotent re-Create minted a new ID: %s vs %s", again.Meta.ID, first.Meta.ID)
	}

	// Deleting one Volume frees enough quota (100-40=60 used) for another.
	if err := svc.DeleteVolume(ctx, tenant, second.Meta.ID); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := svc.CreateVolume(ctx, tenant, "vol-4", testVolumeSpec(30)); err != nil {
		t.Fatalf("CreateVolume after Delete freed quota: %v", err)
	}
}

// TestService_NewServiceRebuildsUsageFromExistingVolumes proves the real
// bug found 2026-09-13 (same class as
// network.Service.rebuildPools/compute.Service.rebuildUsage): usage is
// purely in-memory, so without rebuildUsage, a restart forgets every
// tenant's real quota usage and could let CreateVolume approve a request a
// live tenant_usage would have rejected. Constructs two Services against
// the SAME etcd client/namespace (not resourcetest.Client(t) called
// twice, which would give each its own isolated namespace) to simulate a
// real restart.
func TestService_NewServiceRebuildsUsageFromExistingVolumes(t *testing.T) {
	ctx := context.Background()
	etcdClient := resourcetest.Client(t)
	quota := &identityv1.QuotaSpec{MaxVolumeGb: 100}
	const tenant = "tenant-a"

	svc1, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, nil)
	if err != nil {
		t.Fatalf("NewService (first): %v", err)
	}
	mustCreateTestStorageConnection(t, ctx, svc1, "test-connection", "test-zone")
	if _, err := svc1.CreateVolume(ctx, tenant, "vol-1", testVolumeSpec(60)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	svc2, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, nil)
	if err != nil {
		t.Fatalf("NewService (second, simulating a restart): %v", err)
	}

	// max_volume_gb=100, vol-1 already used 60 -- only 40 more should fit.
	// Without rebuildUsage, svc2 would start from an empty usage map and
	// wrongly allow this.
	if _, err := svc2.CreateVolume(ctx, tenant, "vol-2", testVolumeSpec(50)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("CreateVolume (after restart, over remaining quota): got %v, want ErrQuotaExceeded -- rebuildUsage did not restore usage's state", err)
	}
	if _, err := svc2.CreateVolume(ctx, tenant, "vol-3", testVolumeSpec(30)); err != nil {
		t.Fatalf("CreateVolume (after restart, within remaining quota): %v", err)
	}
}
