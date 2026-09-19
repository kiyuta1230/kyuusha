package compute

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// stoppedVM creates a VM and forces it to Stopped -- like
// stoppedVMWithHypervisor (service_test.go) minus the Hypervisor
// registration/pinning, which AttachVolume/DetachVolume never touch (unlike
// Resize, there's no Hypervisor-capacity accounting for storage).
func stoppedVM(t *testing.T, ctx context.Context, svc *Service, tenant string, spec VirtualMachineSpec) VirtualMachine {
	t.Helper()
	vm, err := svc.Create(ctx, tenant, "", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	vm.Status.Phase = PhaseStopped
	stopped, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}
	return *stopped
}

func TestService_AttachVolumeRequiresStopped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm, err := svc.Create(ctx, tenant, "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.AttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", ""); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("AttachVolume on Pending VM: got %v, want ErrInvalidPhase", err)
	}

	vm.Status.Phase = PhaseRunning
	running, err := svc.Update(ctx, vm)
	if err != nil {
		t.Fatalf("Update to Running: %v", err)
	}
	if _, err := svc.AttachVolume(ctx, tenant, running.Meta.ID, "vol-1", ""); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("AttachVolume on Running VM: got %v, want ErrInvalidPhase", err)
	}

	running.Status.Phase = PhaseStopped
	stopped, err := svc.Update(ctx, running)
	if err != nil {
		t.Fatalf("Update to Stopped: %v", err)
	}

	attached, err := svc.AttachVolume(ctx, tenant, stopped.Meta.ID, "vol-1", "")
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	if len(attached.Spec.Volumes) != 1 || attached.Spec.Volumes[0].VolumeID != "vol-1" {
		t.Fatalf("Spec.Volumes = %+v, want [{VolumeID: vol-1}]", attached.Spec.Volumes)
	}
	if attached.Status.Phase != PhaseStopped {
		t.Fatalf("Phase = %q, want unchanged Stopped", attached.Status.Phase)
	}
}

func TestService_AttachVolumeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVM(t, ctx, svc, tenant, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})

	first, err := svc.AttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", "")
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	second, err := svc.AttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", "")
	if err != nil {
		t.Fatalf("repeat AttachVolume: %v", err)
	}
	if second.Meta.ResourceVersion != first.Meta.ResourceVersion {
		t.Fatalf("ResourceVersion changed on a repeat AttachVolume: %d -> %d", first.Meta.ResourceVersion, second.Meta.ResourceVersion)
	}
	if len(second.Spec.Volumes) != 1 {
		t.Fatalf("Spec.Volumes = %+v, want exactly one entry (not duplicated)", second.Spec.Volumes)
	}
}

func TestService_AttachVolumeRejectsUnknownOrNotReadyVolume(t *testing.T) {
	ctx := context.Background()

	notReady, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{Phase: "Pending"}, &FakeVolumeAttachmentClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"
	vm := stoppedVM(t, ctx, notReady, tenant, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if _, err := notReady.AttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("AttachVolume of a not-Ready Volume: got %v, want ErrValidation", err)
	}

	svc := newTestService(t, ctx)
	vm2 := stoppedVM(t, ctx, svc, tenant, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if _, err := svc.AttachVolume(ctx, tenant, vm2.Meta.ID, "", ""); !errors.Is(err, ErrValidation) {
		t.Fatalf("AttachVolume with empty volume_id: got %v, want ErrValidation", err)
	}
}

func TestService_DetachVolumeRequiresStopped(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVM(t, ctx, svc, tenant, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	attached, err := svc.AttachVolume(ctx, tenant, vm.Meta.ID, "vol-1", "")
	if err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}

	attached.Status.Phase = PhaseRunning
	running, err := svc.Update(ctx, attached)
	if err != nil {
		t.Fatalf("Update to Running: %v", err)
	}
	if _, err := svc.DetachVolume(ctx, tenant, running.Meta.ID, "vol-1"); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("DetachVolume on Running VM: got %v, want ErrInvalidPhase", err)
	}
}

func TestService_DetachVolumeIsIdempotentIfNotAttached(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	const tenant = "tenant-a"

	vm := stoppedVM(t, ctx, svc, tenant, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	out, err := svc.DetachVolume(ctx, tenant, vm.Meta.ID, "vol-never-attached")
	if err != nil {
		t.Fatalf("DetachVolume of an unattached volume: %v", err)
	}
	if out.Meta.ResourceVersion != vm.Meta.ResourceVersion {
		t.Fatalf("ResourceVersion changed on a no-op DetachVolume: %d -> %d", vm.Meta.ResourceVersion, out.Meta.ResourceVersion)
	}
}

// TestService_DetachVolumeDeletesExistingAttachment exercises the eager,
// real VolumeAttachment deletion DetachVolume performs (unlike Resize's
// entirely spec-only mutation) -- see Service.DetachVolume's doc comment
// for why this can't wait for the next Start the way AttachVolume can.
func TestService_DetachVolumeDeletesExistingAttachment(t *testing.T) {
	ctx := context.Background()
	attClient := &FakeVolumeAttachmentClient{}
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, &FakeImageClient{}, &FakeSubnetClient{}, &FakeNetworkInterfaceClient{}, &FakeVolumeClient{}, attClient)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	vm := stoppedVM(t, ctx, svc, tenant, VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512, Volumes: []VolumeRequest{{VolumeID: "vol-1"}}})

	// Simulate a prior Start having gone through createVolumeAttachments
	// (volume.go), which is what would really populate this in production.
	att, err := attClient.Create(ctx, &blockstoragev1.CreateVolumeAttachmentRequest{
		TenantId: tenant,
		Name:     volumeAttachmentName(vm.Meta.ID, "vol-1"),
		Spec:     &blockstoragev1.VolumeAttachmentSpec{VolumeId: "vol-1", VmId: vm.Meta.ID},
	})
	if err != nil {
		t.Fatalf("seed VolumeAttachment: %v", err)
	}
	vm.Status.VolumeAttachmentRefs = []string{att.GetMeta().GetId()}
	seeded, err := svc.Update(ctx, &vm)
	if err != nil {
		t.Fatalf("Update to seed VolumeAttachmentRefs: %v", err)
	}

	out, err := svc.DetachVolume(ctx, tenant, seeded.Meta.ID, "vol-1")
	if err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}
	if len(out.Spec.Volumes) != 0 {
		t.Fatalf("Spec.Volumes = %+v, want empty", out.Spec.Volumes)
	}
	if len(out.Status.VolumeAttachmentRefs) != 0 {
		t.Fatalf("Status.VolumeAttachmentRefs = %+v, want empty", out.Status.VolumeAttachmentRefs)
	}
	if _, err := attClient.Get(ctx, &blockstoragev1.GetVolumeAttachmentRequest{TenantId: tenant, Id: att.GetMeta().GetId()}); status.Code(err) != codes.NotFound {
		t.Fatalf("VolumeAttachment still exists after DetachVolume: got err %v, want NotFound", err)
	}
}

// TestCreateVolumeAttachments_VolumeIDKeyedNameSurvivesMidListDetach is the
// regression test for the naming-scheme bug DetachVolume's design had to
// avoid: three volumes, remove the middle one (as DetachVolume does), then
// re-derive createVolumeAttachments for the remaining two and confirm
// neither gets a new, duplicate attachment.
func TestCreateVolumeAttachments_VolumeIDKeyedNameSurvivesMidListDetach(t *testing.T) {
	ctx := context.Background()
	volumeClient := &FakeVolumeClient{}
	attClient := &FakeVolumeAttachmentClient{}
	const tenant, vmID = "tenant-a", "vm-1"

	volumes := []VolumeRequest{{VolumeID: "vol-a"}, {VolumeID: "vol-b"}, {VolumeID: "vol-c"}}
	_, refs1, err := createVolumeAttachments(ctx, volumeClient, attClient, tenant, vmID, volumes)
	if err != nil {
		t.Fatalf("createVolumeAttachments (initial): %v", err)
	}
	if len(refs1) != 3 {
		t.Fatalf("refs = %v, want 3 attachments", refs1)
	}

	// Simulate DetachVolume removing the middle entry (vol-b) -- both by
	// deleting its real attachment and by shrinking vm.Spec.Volumes, same
	// as Service.DetachVolume does.
	for _, id := range refs1 {
		a, err := attClient.Get(ctx, &blockstoragev1.GetVolumeAttachmentRequest{TenantId: tenant, Id: id})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if a.GetSpec().GetVolumeId() == "vol-b" {
			if _, err := attClient.Delete(ctx, &blockstoragev1.DeleteVolumeAttachmentRequest{TenantId: tenant, Id: id}); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		}
	}
	remaining := []VolumeRequest{{VolumeID: "vol-a"}, {VolumeID: "vol-c"}}

	_, refs2, err := createVolumeAttachments(ctx, volumeClient, attClient, tenant, vmID, remaining)
	if err != nil {
		t.Fatalf("createVolumeAttachments (after detach): %v", err)
	}
	if len(refs2) != 2 {
		t.Fatalf("refs after detach+reconcile = %v, want 2", refs2)
	}
	// vol-a (index 0 before and after) must have kept its original
	// attachment id -- not been recreated under a new name.
	if refs2[0] != refs1[0] {
		t.Fatalf("vol-a's attachment id changed: %s -> %s (should be reused via its legacy name)", refs1[0], refs2[0])
	}
	// vol-c moved from index 2 to index 1: its legacy name no longer
	// matches, so it must have been picked up by the volume_id-keyed name
	// instead of minted as a brand-new duplicate.
	if refs2[1] != refs1[2] {
		t.Fatalf("vol-c's attachment id changed: %s -> %s (should be reused via its volume_id-keyed name, not duplicated)", refs1[2], refs2[1])
	}

	resp, err := attClient.List(ctx, &blockstoragev1.ListVolumeAttachmentsRequest{TenantId: tenant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetItems()) != 2 {
		t.Fatalf("total attachments after detach+reconcile = %d, want 2 (no orphaned duplicates)", len(resp.GetItems()))
	}
}

// TestCreateVolumeAttachments_LegacyPositionalNameStillMatchedForUntouchedVM
// confirms a VM that has never had a volume detached keeps resolving to its
// original volattach-<vm_id>-<index>-named attachment forever -- the
// zero-migration-cost compatibility story for the naming-scheme change.
func TestCreateVolumeAttachments_LegacyPositionalNameStillMatchedForUntouchedVM(t *testing.T) {
	ctx := context.Background()
	volumeClient := &FakeVolumeClient{}
	attClient := &FakeVolumeAttachmentClient{}
	const tenant, vmID = "tenant-a", "vm-1"

	// Pre-seed an attachment under the OLD (pre-2026-09-19) positional
	// name, as if it had been created before this naming change shipped.
	seeded, err := attClient.Create(ctx, &blockstoragev1.CreateVolumeAttachmentRequest{
		TenantId: tenant,
		Name:     legacyVolumeAttachmentName(vmID, 0),
		Spec:     &blockstoragev1.VolumeAttachmentSpec{VolumeId: "vol-a", VmId: vmID},
	})
	if err != nil {
		t.Fatalf("seed legacy attachment: %v", err)
	}

	_, refs, err := createVolumeAttachments(ctx, volumeClient, attClient, tenant, vmID, []VolumeRequest{{VolumeID: "vol-a"}})
	if err != nil {
		t.Fatalf("createVolumeAttachments: %v", err)
	}
	if len(refs) != 1 || refs[0] != seeded.GetMeta().GetId() {
		t.Fatalf("refs = %v, want [%s] (the pre-existing legacy-named attachment reused, not duplicated)", refs, seeded.GetMeta().GetId())
	}

	resp, err := attClient.List(ctx, &blockstoragev1.ListVolumeAttachmentsRequest{TenantId: tenant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.GetItems()) != 1 {
		t.Fatalf("total attachments = %d, want 1 (no duplicate created)", len(resp.GetItems()))
	}
}
