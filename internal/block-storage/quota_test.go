package blockstorage

import (
	"context"
	"errors"
	"testing"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func TestService_CreateVolumeEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, &FakeTenantClient{Quota: &identityv1.QuotaSpec{MaxVolumeGb: 100}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
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
