package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

// This file implements the live (Running+CLOUD_HYPERVISOR) branch of
// Resize/AttachVolume/DetachVolume -- see docs/open-questions.md「cloud-
// hypervisor限定のライブホットプラグ」 for the full design rationale. Every
// method here lives on *Reconciler, not Service, for the exact same reason
// StreamConsole does (console.go): the NATS round trip these need must not
// force Service itself to hold a NATS handle.

const (
	// hotplugTimeout bounds how long a live op blocks waiting for
	// compute-agent's reply. Longer than consoleFirstResponseTimeout's 5s
	// (console.go): a real vm.resize/add-disk/remove-device call does real
	// device-model work (ACPI CPU-online notify, virtio-blk probe), not
	// just "is anyone home".
	hotplugTimeout = 10 * time.Second

	// attachPollTimeout/attachPollInterval bound LiveAttachVolume's wait for
	// its VolumeAttachment to reach Attached. block-storage's tryAttach
	// runs asynchronously in a separate process (its own reconciler, see
	// docs/architecture.md), so this polls rather than blocking on a Watch
	// stream for what's a one-shot wait.
	attachPollTimeout  = 5 * time.Second
	attachPollInterval = 150 * time.Millisecond
)

// ErrLiveOpUnavailable distinguishes "compute-agent didn't answer in time"
// (Unavailable) from a validation error (InvalidArgument, toStatus's
// fallback) or ErrInvalidPhase (FailedPrecondition) -- see grpcserver's
// toStatus.
var ErrLiveOpUnavailable = errors.New("vm: live operation did not complete (compute-agent unreachable or timed out)")

// isLiveEligible reports whether vm qualifies for the live (Running +
// CLOUD_HYPERVISOR) branch of Resize/AttachVolume/DetachVolume -- any other
// phase/driver combination (including Running+FIRECRACKER) takes the cold
// path instead, which then rejects it with ErrInvalidPhase exactly as
// before this feature existed.
func isLiveEligible(vm VirtualMachine) bool {
	return vm.Status.Phase == PhaseRunning && vm.Spec.DriverHint == VmmDriverCloudHypervisor
}

// roundTripHotplug publishes cmd to hypervisor's COMPUTE_CMD (at-least-once/
// ack-on-receipt, same durability contract as StopCommand) and blocks for
// its HotplugResult on a fresh per-request reply-subject inbox (same
// pattern as ConsoleRequest, console.go) -- not COMPUTE_EVT, since only
// this one caller ever cares about the result.
func (r *Reconciler) roundTripHotplug(ctx context.Context, hypervisor string, cmd HotplugCommand) error {
	replySubject := r.nc.NewInbox()
	cmd.ReplySubject = replySubject
	sub, err := r.nc.SubscribeSync(replySubject)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	payload, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	msg := nats.NewMsg(CmdSubjectHotplug(hypervisor))
	msg.Data = payload
	if _, err := r.js.PublishMsg(ctx, msg); err != nil {
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, hotplugTimeout)
	defer cancel()
	reply, err := sub.NextMsgWithContext(waitCtx)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrLiveOpUnavailable, err)
	}
	var res HotplugResult
	if err := json.Unmarshal(reply.Data, &res); err != nil {
		return err
	}
	if !res.Success {
		return fmt.Errorf("%w: %s", ErrValidation, res.Error)
	}
	return nil
}

// LiveResize changes a Running+CLOUD_HYPERVISOR VM's vcpu/memory without a
// reboot, via cloud-hypervisor's --api-socket (see
// internal/compute-agent/chvmm's Hotplugger implementation). Reuses the
// same quota/Hypervisor-capacity reservation Resize's cold path uses --
// only the actual resize step (a direct api-socket call, instead of just
// persisting the new spec) differs.
func (r *Reconciler) LiveResize(ctx context.Context, tenantID, id string, vcpu int32, memoryMB int64) (*VirtualMachine, error) {
	if vcpu <= 0 {
		return nil, fmt.Errorf("%w: vcpu must be positive", ErrValidation)
	}
	if memoryMB <= 0 {
		return nil, fmt.Errorf("%w: memory_mb must be positive", ErrValidation)
	}

	r.svc.usageMu.Lock()
	defer r.svc.usageMu.Unlock()

	vm, err := r.svc.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if !isLiveEligible(vm) {
		return nil, fmt.Errorf("%w: live resize requires Running+CLOUD_HYPERVISOR (phase=%s, driver=%s)", ErrInvalidPhase, vm.Status.Phase, vm.Spec.DriverHint)
	}
	if vcpu == vm.Spec.VCPU && memoryMB == vm.Spec.MemoryMB {
		return &vm, nil
	}

	deltaVCPU := vcpu - vm.Spec.VCPU
	deltaMemoryMB := memoryMB - vm.Spec.MemoryMB

	if deltaVCPU > 0 || deltaMemoryMB > 0 {
		limit, err := lookupQuota(ctx, r.svc.identityClient, tenantID)
		if err != nil {
			return nil, err
		}
		usage := r.svc.usage[tenantID]
		allowed, err := r.svc.quota.allowResize(ctx, usage, deltaVCPU, deltaMemoryMB, vcpu, memoryMB, limit)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, fmt.Errorf("%w: tenant %q", ErrQuotaExceeded, tenantID)
		}
	}

	if err := r.svc.resizeHypervisorCapacity(ctx, vm.Status.Hypervisor, deltaVCPU, deltaMemoryMB); err != nil {
		return nil, err
	}

	hpErr := r.roundTripHotplug(ctx, vm.Status.Hypervisor, HotplugCommand{
		VMID: id, TenantID: tenantID, DriverHint: string(vm.Spec.DriverHint),
		Op: HotplugOpResize, VCPU: vcpu, MemoryMB: memoryMB,
	})
	if hpErr != nil {
		r.svc.releaseHypervisorCapacity(ctx, vm.Status.Hypervisor, deltaVCPU, deltaMemoryMB)
		return nil, hpErr
	}

	prevVCPU, prevMemoryMB := vm.Spec.VCPU, vm.Spec.MemoryMB
	vm.Spec.VCPU = vcpu
	vm.Spec.MemoryMB = memoryMB
	out, err := r.svc.store.Update(ctx, vm)
	if err != nil {
		// The guest has already been live-resized at this point -- best-
		// effort compensating resize-back so the guest and the (still old)
		// persisted spec agree again, then release the capacity delta.
		// Same "log and move on" Saga-style philosophy
		// releaseHypervisorCapacity itself already documents; a residual
		// failure here (both calls failing) is a known, documented gap.
		if backErr := r.roundTripHotplug(ctx, vm.Status.Hypervisor, HotplugCommand{
			VMID: id, TenantID: tenantID, DriverHint: string(vm.Spec.DriverHint),
			Op: HotplugOpResize, VCPU: prevVCPU, MemoryMB: prevMemoryMB,
		}); backErr != nil {
			slog.Error("live resize: compensating resize-back failed, guest and persisted spec are now out of sync", "vm_id", id, "err", backErr)
		}
		r.svc.releaseHypervisorCapacity(ctx, vm.Status.Hypervisor, deltaVCPU, deltaMemoryMB)
		return nil, err
	}

	usage := r.svc.usage[tenantID]
	usage.VCPU += deltaVCPU
	usage.MemoryMB += deltaMemoryMB
	r.svc.usage[tenantID] = usage
	return &out, nil
}

// LiveAttachVolume attaches an already-Ready Volume to a Running+
// CLOUD_HYPERVISOR VM without a reboot. Creates (idempotently) the
// VolumeAttachment and waits for it to reach Attached BEFORE hotplugging
// the device into the guest: the reverse order would let the guest start
// using a device block-storage doesn't yet consider locked, risking a
// second VM concurrently attaching the same Volume (real data corruption
// from two guests writing to one disk) -- attachment-first's worst case is
// only a recoverable orphaned lock if the hotplug step then fails.
func (r *Reconciler) LiveAttachVolume(ctx context.Context, tenantID, id, volumeID, deviceHint string) (*VirtualMachine, error) {
	if volumeID == "" {
		return nil, fmt.Errorf("%w: volume_id is required", ErrValidation)
	}

	vm, err := r.svc.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	for _, v := range vm.Spec.Volumes {
		if v.VolumeID == volumeID {
			return &vm, nil // idempotent no-op, same shape as the cold path
		}
	}
	if !isLiveEligible(vm) {
		return nil, fmt.Errorf("%w: live attach requires Running+CLOUD_HYPERVISOR (phase=%s, driver=%s)", ErrInvalidPhase, vm.Status.Phase, vm.Spec.DriverHint)
	}
	if err := validateVolumes(ctx, r.svc.volumeClient, tenantID, []VolumeRequest{{VolumeID: volumeID, DeviceHint: deviceHint}}); err != nil {
		return nil, err
	}

	// Idempotent-by-name, same helper the cold path's provisionAndPublish
	// uses (volume.go) -- reuses the exact same deterministic naming and
	// exclusive-attach semantics rather than inventing a parallel path.
	_, refs, err := createVolumeAttachments(ctx, r.svc.volumeClient, r.svc.volumeAttachmentClient, tenantID, id, []VolumeRequest{{VolumeID: volumeID, DeviceHint: deviceHint}})
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("%w: failed to create volume attachment", ErrValidation)
	}
	attachmentID := refs[0]

	var att *blockstoragev1.VolumeAttachment
	deadline := time.Now().Add(attachPollTimeout)
	for {
		att, err = r.svc.volumeAttachmentClient.Get(ctx, &blockstoragev1.GetVolumeAttachmentRequest{TenantId: tenantID, Id: attachmentID})
		if err != nil {
			return nil, err
		}
		if att.GetStatus().GetPhase() == "Attached" {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: volume %q did not reach Attached (likely held elsewhere)", ErrValidation, volumeID)
		}
		time.Sleep(attachPollInterval)
	}

	vol, err := r.svc.volumeClient.Get(ctx, &blockstoragev1.GetVolumeRequest{TenantId: tenantID, Id: volumeID})
	if err != nil {
		return nil, err
	}

	hpErr := r.roundTripHotplug(ctx, vm.Status.Hypervisor, HotplugCommand{
		VMID: id, TenantID: tenantID, DriverHint: string(vm.Spec.DriverHint),
		Op: HotplugOpAddDisk, DeviceID: attachmentID,
		Protocol: vol.GetSpec().GetProtocol().String(), StorageConnection: vol.GetSpec().GetStorageConnection(),
		Identifier: vol.GetSpec().GetIdentifier(), SizeGB: vol.GetSpec().GetSizeGb(),
	})
	if hpErr != nil {
		// Best-effort: release the lock this call just took, so the Volume
		// isn't left permanently unattachable -- same "don't leave a stale
		// lock behind" philosophy DetachVolume's eager delete documents.
		if _, delErr := r.svc.volumeAttachmentClient.Delete(ctx, &blockstoragev1.DeleteVolumeAttachmentRequest{TenantId: tenantID, Id: attachmentID}); delErr != nil {
			slog.Warn("live attach: cleanup of orphaned VolumeAttachment after hotplug failure also failed", "attachment_id", attachmentID, "err", delErr)
		}
		return nil, hpErr
	}

	vm.Spec.Volumes = append(vm.Spec.Volumes, VolumeRequest{VolumeID: volumeID, DeviceHint: deviceHint})
	vm.Status.VolumeAttachmentRefs = append(vm.Status.VolumeAttachmentRefs, attachmentID)
	out, err := r.svc.store.Update(ctx, vm)
	if err != nil {
		// Guest and block-storage are already correctly attached at this
		// point -- only vm.Spec/Status is stale. No compensating hotplug
		// removal needed (unlike LiveResize, where guest state itself
		// needed reverting): a retried AttachVolume finds the attachment
		// already Attached (idempotent) and only needs to retry add-disk
		// (tolerated as a duplicate-id no-op, see chvmm.Manager.LiveAddDisk)
		// and this Update.
		return nil, err
	}
	return &out, nil
}

// LiveDetachVolume detaches a Volume from a Running+CLOUD_HYPERVISOR VM
// without a reboot. Mirror image of LiveAttachVolume's ordering: unplug
// from the guest FIRST, while the guest is still the only writer, THEN
// release block-storage's exclusive-attach lock -- releasing the lock
// before the guest actually stops using the device would let another VM
// attach the same Volume while this guest still has it live.
func (r *Reconciler) LiveDetachVolume(ctx context.Context, tenantID, id, volumeID string) (*VirtualMachine, error) {
	vm, err := r.svc.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i, v := range vm.Spec.Volumes {
		if v.VolumeID == volumeID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return &vm, nil // idempotent no-op
	}
	if !isLiveEligible(vm) {
		return nil, fmt.Errorf("%w: live detach requires Running+CLOUD_HYPERVISOR (phase=%s, driver=%s)", ErrInvalidPhase, vm.Status.Phase, vm.Spec.DriverHint)
	}

	var attachmentID string
	var remainingRefs []string
	for _, ref := range vm.Status.VolumeAttachmentRefs {
		a, err := r.svc.volumeAttachmentClient.Get(ctx, &blockstoragev1.GetVolumeAttachmentRequest{TenantId: tenantID, Id: ref})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				continue // already gone (e.g. orphan-GC'd)
			}
			return nil, err
		}
		if a.GetSpec().GetVolumeId() == volumeID {
			attachmentID = ref
			continue // drop from remainingRefs below regardless of outcome
		}
		remainingRefs = append(remainingRefs, ref)
	}

	if attachmentID == "" {
		// Ref already gone; just drop the spec entry, same as the cold path.
		vm.Spec.Volumes = append(vm.Spec.Volumes[:idx], vm.Spec.Volumes[idx+1:]...)
		vm.Status.VolumeAttachmentRefs = remainingRefs
		out, err := r.svc.store.Update(ctx, vm)
		if err != nil {
			return nil, err
		}
		return &out, nil
	}

	if hpErr := r.roundTripHotplug(ctx, vm.Status.Hypervisor, HotplugCommand{
		VMID: id, TenantID: tenantID, DriverHint: string(vm.Spec.DriverHint),
		Op: HotplugOpRemoveDevice, DeviceID: attachmentID,
	}); hpErr != nil {
		return nil, hpErr // nothing else touched yet; caller can just retry
	}

	if _, err := r.svc.volumeAttachmentClient.Delete(ctx, &blockstoragev1.DeleteVolumeAttachmentRequest{TenantId: tenantID, Id: attachmentID}); err != nil {
		// Guest already detached; retry will find remove-device reports
		// "not found" (tolerated as success, see chvmm.Manager.
		// LiveRemoveDevice) and retry just this Delete.
		return nil, err
	}

	vm.Spec.Volumes = append(vm.Spec.Volumes[:idx], vm.Spec.Volumes[idx+1:]...)
	vm.Status.VolumeAttachmentRefs = remainingRefs
	out, err := r.svc.store.Update(ctx, vm)
	if err != nil {
		// Guest+block-storage already correctly detached; a retry hits the
		// existing NotFound-Get tolerance above and self-heals.
		return nil, err
	}
	return &out, nil
}
