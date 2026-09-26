package image

import (
	"context"
	"errors"
	"testing"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestService_CreateRejectsUnknownTenant(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := svc.Create(ctx, "", "x", Spec{Format: FormatKernelRootfs, Kernel: Artifact{URL: "http://x/kernel"}, Rootfs: Artifact{URL: "http://x/rootfs"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty tenant_id: got %v, want ErrValidation", err)
	}
}

func TestService_CreateEnforcesImageQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{MaxImages: 2}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	spec := Spec{Format: FormatKernelRootfs, Kernel: Artifact{URL: "http://x/kernel"}, Rootfs: Artifact{URL: "http://x/rootfs"}}

	first, err := svc.Create(ctx, tenant, "image-1", spec)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := svc.Create(ctx, tenant, "image-2", spec); err != nil {
		t.Fatalf("second Create (within quota): %v", err)
	}
	if _, err := svc.Create(ctx, tenant, "image-3", spec); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("third Create over max_images=2: got %v, want ErrQuotaExceeded", err)
	}

	// Idempotent re-Create of an existing name must not re-charge quota or
	// be rejected by the already-exhausted quota.
	again, err := svc.Create(ctx, tenant, "image-1", spec)
	if err != nil {
		t.Fatalf("idempotent re-Create: %v", err)
	}
	if again.Meta.ID != first.Meta.ID {
		t.Fatalf("idempotent re-Create minted a new ID: %s vs %s", again.Meta.ID, first.Meta.ID)
	}

	// Deleting one Image frees enough quota for another.
	if err := svc.Delete(ctx, tenant, first.Meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Create(ctx, tenant, "image-4", spec); err != nil {
		t.Fatalf("Create after Delete freed quota: %v", err)
	}
}

// TestService_NewServiceRebuildsUsageFromExistingImages proves the same
// class of bug found 2026-09-13 in compute/block-storage/network's
// rebuildUsage/rebuildPools: usage is purely in-memory, so without
// rebuildUsage, a restart forgets every tenant's real usage. Constructs two
// Services against the SAME etcd client/namespace (not
// resourcetest.Client(t) called twice, which would give each its own
// isolated namespace) to simulate a real restart.
func TestService_NewServiceRebuildsUsageFromExistingImages(t *testing.T) {
	ctx := context.Background()
	etcdClient := resourcetest.Client(t)
	quota := &identityv1.QuotaSpec{MaxImages: 1}
	const tenant = "tenant-a"
	spec := Spec{Format: FormatKernelRootfs, Kernel: Artifact{URL: "http://x/kernel"}, Rootfs: Artifact{URL: "http://x/rootfs"}}

	svc1, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota})
	if err != nil {
		t.Fatalf("NewService (first): %v", err)
	}
	if _, err := svc1.Create(ctx, tenant, "image-1", spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc2, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota})
	if err != nil {
		t.Fatalf("NewService (second, simulating a restart): %v", err)
	}

	if _, err := svc2.Create(ctx, tenant, "image-2", spec); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Create (after restart, over max_images=1): got %v, want ErrQuotaExceeded -- rebuildUsage did not restore usage's state", err)
	}
}
