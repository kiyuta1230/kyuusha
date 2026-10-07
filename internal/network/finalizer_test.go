package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestService_SubnetFinalizerHoldsVLAN covers Finalizers on Subnet end to
// end: a held Subnet keeps its VLAN ID (and is never handed out again)
// until its last finalizer is removed, only the finalizer's owner can
// remove it, and tenant_usage drops exactly once.
func TestService_SubnetFinalizerHoldsVLAN(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go svc.watchPendingSubnets(watchCtx)
	netID := carveNetwork(t, ctx, svc, "tenant-a")

	sn := createAndWaitReady(t, ctx, svc, "tenant-a", "held", SubnetSpec{NetworkID: netID, Zone: "zone-f"})
	heldVLAN := sn.Status.Values["vlan_id"]
	if heldVLAN == 0 {
		t.Fatal("Subnet got no VLAN ID")
	}

	aliceCtx := authn.ContextWithPropagatedCallerForTest(ctx, "alice", false)
	current, _ := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	current.Meta.Finalizers = []resource.Finalizer{{Name: "vpc.example.com/cleanup"}}
	if _, err := svc.UpdateSubnet(aliceCtx, current); err != nil {
		t.Fatalf("UpdateSubnet adding a finalizer: %v", err)
	}
	usageBefore := svc.usage["tenant-a"].SubnetCount

	for i := 0; i < 2; i++ { // the second Delete must not count down again
		if err := svc.DeleteSubnet(ctx, "tenant-a", sn.Meta.ID); err != nil {
			t.Fatalf("DeleteSubnet #%d: %v", i+1, err)
		}
	}
	if got := svc.usage["tenant-a"].SubnetCount; got != usageBefore-1 {
		t.Fatalf("SubnetCount after two Deletes = %d, want %d", got, usageBefore-1)
	}
	lingering, err := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	if err != nil || lingering.Meta.DeletedAt == nil || lingering.Status.Values["vlan_id"] != heldVLAN {
		t.Fatalf("held Subnet = %+v, %v; want it still present, deleted_at set, VLAN %d kept", lingering, err, heldVLAN)
	}

	// While held, its VLAN ID is not handed to anyone else.
	other := createAndWaitReady(t, ctx, svc, "tenant-a", "other", SubnetSpec{NetworkID: netID, Zone: "zone-f"})
	if other.Status.Values["vlan_id"] == heldVLAN {
		t.Fatalf("a new Subnet got VLAN %d while the held Subnet still exists", heldVLAN)
	}

	// Only the finalizer's owner may remove it.
	bobCtx := authn.ContextWithPropagatedCallerForTest(ctx, "bob", false)
	lingering.Meta.Finalizers = nil
	if _, err := svc.UpdateSubnet(bobCtx, lingering); !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateSubnet removing alice's finalizer as bob: got %v, want ErrValidation", err)
	}
	lingering, _ = svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	lingering.Meta.Finalizers = nil
	if _, err := svc.UpdateSubnet(aliceCtx, lingering); err != nil {
		t.Fatalf("UpdateSubnet removing the finalizer as alice: %v", err)
	}
	if _, err := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID); !errors.Is(err, ErrSubnetNotFound) {
		t.Fatalf("Subnet after its last finalizer was removed: got %v, want ErrSubnetNotFound", err)
	}

	// Now (once the watch sees the Deleted event) its VLAN ID is reusable.
	for deadline := time.Now().Add(2 * time.Second); ; {
		probe := createAndWaitReady(t, ctx, svc, "tenant-a", "probe-"+time.Now().Format("150405.000000"), SubnetSpec{NetworkID: netID, Zone: "zone-f"})
		if probe.Status.Values["vlan_id"] == heldVLAN {
			return
		}
		if err := svc.DeleteSubnet(ctx, "tenant-a", probe.Meta.ID); err != nil {
			t.Fatalf("DeleteSubnet probe: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("VLAN %d never became reusable after the held Subnet was really removed", heldVLAN)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// createAndWaitReady creates a Subnet and waits for the already-running
// watchPendingSubnets to allocate its VLAN ID (mustCreateAndAllocateSubnet
// would race that watch for the allocation).
func createAndWaitReady(t *testing.T, ctx context.Context, svc *Service, tenantID, name string, spec SubnetSpec) *Subnet {
	t.Helper()
	sn, err := svc.CreateSubnet(ctx, tenantID, name, spec)
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got, err := svc.GetSubnet(ctx, tenantID, sn.Meta.ID); err == nil && got.Status.Phase == SubnetPhaseReady {
			return got
		}
	}
	t.Fatalf("Subnet %s never went Ready", name)
	return nil
}
