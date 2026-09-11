package image

import (
	"context"
	"testing"
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resourcetest"
)

func kernelRootfsSpec() Spec {
	return Spec{Format: FormatKernelRootfs, Kernel: Artifact{URL: "http://x/kernel"}, Rootfs: Artifact{URL: "http://x/rootfs"}}
}

func TestService_CreateDefaultsVisibilityToPrivate(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	img, err := svc.Create(ctx, "tenant-a", "x", kernelRootfsSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if img.Spec.Visibility != VisibilityPrivate {
		t.Fatalf("Visibility = %q, want PRIVATE (the default)", img.Spec.Visibility)
	}
}

func TestService_CreateRejectsGarbageVisibility(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	spec := kernelRootfsSpec()
	spec.Visibility = Visibility("NOT_A_REAL_VALUE")
	if _, err := svc.Create(ctx, "tenant-a", "x", spec); err == nil {
		t.Fatal("expected validation error for a garbage visibility value")
	}
}

func TestService_GetHidesPrivateImageFromOtherTenants(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	img, err := svc.Create(ctx, "tenant-a", "x", kernelRootfsSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.Get(ctx, "tenant-b", img.Meta.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant Get of a PRIVATE, unshared Image: got %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(ctx, "tenant-a", img.Meta.ID); err != nil {
		t.Fatalf("owner's own Get: %v", err)
	}
}

func TestService_GetAllowsPublicImageFromAnyTenant(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	spec := kernelRootfsSpec()
	spec.Visibility = VisibilityPublic
	img, err := svc.Create(ctx, "tenant-a", "x", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.Get(ctx, "tenant-b", img.Meta.ID)
	if err != nil {
		t.Fatalf("cross-tenant Get of a PUBLIC Image: %v", err)
	}
	if got.Meta.TenantID != "tenant-a" {
		t.Fatalf("TenantID = %q, want the owning tenant unchanged", got.Meta.TenantID)
	}
}

func TestService_GetAllowsPrivateImageSharedWithSpecificTenant(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	spec := kernelRootfsSpec()
	spec.SharedWithTenantIDs = []string{"tenant-b"}
	img, err := svc.Create(ctx, "tenant-a", "x", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.Get(ctx, "tenant-b", img.Meta.ID); err != nil {
		t.Fatalf("Get by a tenant named in shared_with_tenant_ids: %v", err)
	}
	if _, err := svc.Get(ctx, "tenant-c", img.Meta.ID); err != ErrNotFound {
		t.Fatalf("Get by an unrelated tenant: got %v, want ErrNotFound", err)
	}
}

func TestService_ListIncludesOwnAndVisibleImagesOnly(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	own, err := svc.Create(ctx, "tenant-b", "own", kernelRootfsSpec())
	if err != nil {
		t.Fatalf("Create own: %v", err)
	}
	publicSpec := kernelRootfsSpec()
	publicSpec.Visibility = VisibilityPublic
	pub, err := svc.Create(ctx, "tenant-a", "public", publicSpec)
	if err != nil {
		t.Fatalf("Create public: %v", err)
	}
	sharedSpec := kernelRootfsSpec()
	sharedSpec.SharedWithTenantIDs = []string{"tenant-b"}
	shared, err := svc.Create(ctx, "tenant-a", "shared", sharedSpec)
	if err != nil {
		t.Fatalf("Create shared: %v", err)
	}
	if _, err := svc.Create(ctx, "tenant-a", "private", kernelRootfsSpec()); err != nil {
		t.Fatalf("Create private: %v", err)
	}

	imgs, err := svc.List(ctx, "tenant-b")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make(map[string]bool)
	for _, img := range imgs {
		got[img.Meta.ID] = true
	}
	for _, want := range []string{own.Meta.ID, pub.Meta.ID, shared.Meta.ID} {
		if !got[want] {
			t.Fatalf("List(tenant-b) missing expected Image %s: %+v", want, imgs)
		}
	}
	if len(imgs) != 3 {
		t.Fatalf("List(tenant-b) returned %d Images (%+v), want exactly 3 (own + public + shared, not tenant-a's plain private one)", len(imgs), imgs)
	}
}

func TestService_SetVisibilityIsOwnerOnly(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	img, err := svc.Create(ctx, "tenant-a", "x", kernelRootfsSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.SetVisibility(ctx, "tenant-b", img.Meta.ID, VisibilityPublic, nil); err != ErrNotFound {
		t.Fatalf("SetVisibility by a non-owner: got %v, want ErrNotFound", err)
	}

	updated, err := svc.SetVisibility(ctx, "tenant-a", img.Meta.ID, VisibilityPublic, nil)
	if err != nil {
		t.Fatalf("SetVisibility by the owner: %v", err)
	}
	if updated.Spec.Visibility != VisibilityPublic {
		t.Fatalf("Visibility after SetVisibility = %q, want PUBLIC", updated.Spec.Visibility)
	}

	if _, err := svc.Get(ctx, "tenant-b", img.Meta.ID); err != nil {
		t.Fatalf("Get after making PUBLIC: %v", err)
	}
}

func TestService_WatchFiltersOutInvisibleCrossTenantEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	svc := NewService(resourcetest.Client(t))

	events, err := svc.Watch(ctx, "tenant-b", 0)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Invisible to tenant-b: must never appear on its Watch.
	if _, err := svc.Create(ctx, "tenant-a", "private", kernelRootfsSpec()); err != nil {
		t.Fatalf("Create private: %v", err)
	}
	// Visible to tenant-b (PUBLIC): must appear.
	publicSpec := kernelRootfsSpec()
	publicSpec.Visibility = VisibilityPublic
	pub, err := svc.Create(ctx, "tenant-a", "public", publicSpec)
	if err != nil {
		t.Fatalf("Create public: %v", err)
	}

	select {
	case e := <-events:
		if e.Object.Meta.ID != pub.Meta.ID {
			t.Fatalf("first event seen by tenant-b's Watch = %s, want the PUBLIC Image %s (the PRIVATE one should have been filtered out)", e.Object.Meta.ID, pub.Meta.ID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the PUBLIC Image's event")
	}
}
