package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

var tracer = otel.Tracer("github.com/kiyuta1230/kyuusha/internal/compute")

// retrySweepInterval implements docs/architecture.md's "Pendingのまま...
// 報告し続け" retry: reconcile() only runs off VM Watch events, so a VM that
// failed to schedule (no Hypervisor had room) would otherwise never be
// retried once capacity frees up elsewhere, since freeing capacity is a
// Hypervisor change, not a VM change, and doesn't appear on this Watch.
// runRetrySweep also uses this interval as its poll rate for the separate,
// staleness-gated Provisioning/Stopping retry below (stuckPhaseThreshold).
const retrySweepInterval = 10 * time.Second

// stuckPhaseThreshold is how long a VM must sit in Provisioning or Stopping
// before runRetrySweep resends its command (unlike Pending above, which is
// retried unconditionally every tick) -- long enough that a VM taking its
// normal course (a real Boot/Stop only takes a few seconds end to end, see
// fcvmm/chvmm's bootGracePeriod) is never resent, since a resend -- while
// safe, see below -- is still a redundant NATS round trip for a VM that was
// never actually stuck.
//
// Deliberately scoped to "the compute-agent this VM is already scheduled
// onto either never got the message, or restarted and lost track of
// reporting back" -- NOT "the hypervisor itself is gone"
// (see [[kyuusha_selfheal_dropped_pet_cattle]]): resending to a genuinely-
// dead hypervisor's subject just queues in NATS with nothing to consume it,
// same as today, no fencing/rescheduling risk.
//
// Safe to resend at all only because fcvmm/chvmm's Boot is now idempotent
// against an already-running vm_id (see vmm.BootRecord and each driver's
// Reconcile) -- before that fix, resending Create risked starting a
// second, duplicate process. Resending is also what actually advances a
// stuck VM's phase: Boot's idempotent no-op still flows through
// handleCreate's normal path, which still publishes a CreateResult (and
// handleStop *always* publishes a StopResult, success or not) -- the
// resend itself is the retry that finally answers back, not just a safety
// check with nothing left to do.
const stuckPhaseThreshold = 30 * time.Second

// Reconciler drives VirtualMachines from Pending through Provisioning by
// talking to compute-agent over NATS, scheduling them onto real, self-
// registered Hypervisors (see hypervisor_service.go).
type Reconciler struct {
	svc *Service
	nc  *nats.Conn
	js  jetstream.JetStream
}

func NewReconciler(svc *Service, nc *nats.Conn, js jetstream.JetStream) *Reconciler {
	return &Reconciler{svc: svc, nc: nc, js: js}
}

// Run blocks, reconciling VMs and Hypervisor heartbeats/health until ctx is
// done.
func (r *Reconciler) Run(ctx context.Context) error {
	if err := EnsureStreams(ctx, r.js); err != nil {
		return err
	}

	if err := r.consumeResults(ctx); err != nil {
		return fmt.Errorf("consume results: %w", err)
	}
	if err := r.consumeStopResults(ctx); err != nil {
		return fmt.Errorf("consume stop results: %w", err)
	}
	if err := r.subscribeHeartbeats(); err != nil {
		return fmt.Errorf("subscribe heartbeats: %w", err)
	}
	go r.svc.runHealthSweep(ctx)
	go r.runRetrySweep(ctx)
	go r.publishHypervisorStorageConnections(ctx)

	events, err := r.svc.Watch(ctx, "", 0, "") // all tenants, unfiltered: internal use only
	if err != nil {
		return fmt.Errorf("watch vms: %w", err)
	}
	for e := range events {
		switch e.Type {
		case EventAdded, EventModified:
			r.reconcile(ctx, e.Object)
		case EventDeleted:
			r.releaseIfReserved(ctx, e.Object)
		}
	}
	return nil
}

// runRetrySweep drives two independent retries off the same periodic VM
// listing: Pending (unconditional every tick, see retrySweepInterval) and
// Provisioning/Stopping (only once stuck past stuckPhaseThreshold -- see
// its doc comment for why this is now safe and why it's scoped the way it
// is). stuckSince is owned entirely by this goroutine (nothing else reads
// or writes it), so no locking is needed despite living across ticks.
func (r *Reconciler) runRetrySweep(ctx context.Context) {
	stuckSince := make(map[string]time.Time) // vm_id -> first tick this process observed it stuck in Provisioning/Stopping
	ticker := time.NewTicker(retrySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			vms, err := r.svc.List(ctx, "")
			if err != nil {
				continue
			}
			seenStuck := make(map[string]bool, len(vms))
			for _, vm := range vms {
				switch vm.Status.Phase {
				case PhasePending, PhaseMigrating:
					// PhaseMigrating fails the exact same way PhasePending
					// can (scheduleMigration/scheduleVM both return
					// ErrUnschedulable when nothing currently fits), and
					// needs the same unconditional-every-tick retry: a
					// Hypervisor freeing capacity is a Hypervisor change,
					// not a VM change, so it never appears on this VM's own
					// Watch stream either.
					r.reconcile(ctx, vm)
				case PhaseProvisioning, PhaseStopping:
					seenStuck[vm.Meta.ID] = true
					since, ok := stuckSince[vm.Meta.ID]
					if !ok {
						stuckSince[vm.Meta.ID] = time.Now()
						continue
					}
					if time.Since(since) < stuckPhaseThreshold {
						continue
					}
					slog.Warn("retry sweep: VM stuck past threshold, resending its command", "vm_id", vm.Meta.ID, "phase", vm.Status.Phase, "stuck_for", time.Since(since))
					if vm.Status.Phase == PhaseProvisioning {
						r.provisionAndPublish(ctx, vm)
					} else {
						r.publishStopCommand(ctx, vm)
					}
					// Reset rather than delete: retry again only after
					// another full threshold if it's still stuck next tick,
					// not on every subsequent tick.
					stuckSince[vm.Meta.ID] = time.Now()
				}
			}
			// A vm_id no longer seen in Provisioning/Stopping (progressed,
			// or deleted) shouldn't keep counting toward a future stuck
			// window if it ever re-enters that phase later (e.g. Start
			// after Stop going through Provisioning again).
			for id := range stuckSince {
				if !seenStuck[id] {
					delete(stuckSince, id)
				}
			}
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context, vm VirtualMachine) {
	switch vm.Status.Phase {
	case PhasePending:
		// Re-derive the required zone from spec.network_interfaces' Subnets
		// now rather than trusting anything cached from Create time: a Subnet
		// could have changed (or been deleted) in the meantime, and this is
		// the same validation Create already ran, just re-run immediately
		// before scheduling so the zone constraint is always fresh.
		zone, err := validateNetworkInterfaces(ctx, r.svc.subnetClient, vm.Meta.TenantID, vm.Spec.NetworkInterfaces)
		if err != nil {
			vm.Status.Conditions = upsertCondition(vm.Status.Conditions, resource.Condition{
				Type:             "Unschedulable",
				Status:           resource.ConditionTrue,
				Reason:           "NetworkInterfaceInvalid",
				Message:          err.Error(),
				LastTransitionAt: time.Now(),
			})
			if _, uerr := r.svc.Update(ctx, &vm); uerr != nil {
				slog.Error("unschedulable: report condition failed", "vm_id", vm.Meta.ID, "err", uerr)
			}
			return
		}

		hypervisorID, err := r.svc.scheduleVM(ctx, vm.Spec, zone, "")
		if err != nil {
			vm.Status.Conditions = upsertCondition(vm.Status.Conditions, resource.Condition{
				Type:             "Unschedulable",
				Status:           resource.ConditionTrue,
				Reason:           "InsufficientCapacity",
				Message:          err.Error(),
				LastTransitionAt: time.Now(),
			})
			if _, uerr := r.svc.Update(ctx, &vm); uerr != nil {
				slog.Error("unschedulable: report condition failed", "vm_id", vm.Meta.ID, "err", uerr)
			}
			return
		}

		vm.Status.Phase = PhaseScheduled
		vm.Status.Hypervisor = hypervisorID
		vm.Status.Conditions = upsertCondition(vm.Status.Conditions, resource.Condition{
			Type:             "Unschedulable",
			Status:           resource.ConditionFalse,
			Reason:           "Scheduled",
			LastTransitionAt: time.Now(),
		})
		if _, err := r.svc.Update(ctx, &vm); err != nil {
			slog.Error("schedule: update failed", "vm_id", vm.Meta.ID, "err", err)
			r.svc.releaseHypervisorCapacity(ctx, hypervisorID, vm.Spec.VCPU, vm.Spec.MemoryMB)
		}

	case PhaseScheduled, PhaseStarting:
		// PhaseStarting (Service.Start, a Stopped VM) reuses this exact same
		// body as a freshly-Scheduled VM: vm.Status.Hypervisor is already set
		// either way (a fresh schedule just set it above; a restarting VM
		// kept it across Stop, since Stop never releases the reservation --
		// see Service.Stop), and createNetworkInterfaces/
		// createVolumeAttachments below are idempotent by name, so for a
		// restart they simply refetch (and re-validate the freshness of) the
		// same NetworkInterfaces/VolumeAttachments this VM already had rather
		// than creating new ones.
		r.provisionAndPublish(ctx, vm)

	case PhaseStopping:
		r.publishStopCommand(ctx, vm)

	case PhaseMigrating:
		r.migrateVM(ctx, vm)
	}
}

// migrateVM (PhaseMigrating, from Service.Migrate) schedules vm onto a
// different Hypervisor -- excluding its current one, or validating
// Status.MigrateTarget if the caller specified one (scheduleMigration) --
// releases the old Hypervisor's capacity reservation, tells its
// compute-agent to tear down whatever this vm_id left behind there (fire-
// and-forget DeleteCommand, same as releaseIfReserved: safe because Migrate
// only ever runs against a Stopped VM, so there's no live process to tear
// down, just an orphaned jail/run directory holding the old root disk --
// see docs/specs/virtual-machine.md「マイグレーション」for why that content
// is deliberately not preserved), and hands off to the exact same
// Scheduled-phase body a fresh Create uses (provisionAndPublish):
// createNetworkInterfaces/createVolumeAttachments are idempotent by the
// deterministic iface-<vm-id>-*/volattach-<vm-id>-* names this VM already
// has, so its IP/MAC and Volume data carry over unchanged onto the new
// Hypervisor.
func (r *Reconciler) migrateVM(ctx context.Context, vm VirtualMachine) {
	zone, err := validateNetworkInterfaces(ctx, r.svc.subnetClient, vm.Meta.TenantID, vm.Spec.NetworkInterfaces)
	if err != nil {
		vm.Status.Conditions = upsertCondition(vm.Status.Conditions, resource.Condition{
			Type:             "Unmigratable",
			Status:           resource.ConditionTrue,
			Reason:           "NetworkInterfaceInvalid",
			Message:          err.Error(),
			LastTransitionAt: time.Now(),
		})
		if _, uerr := r.svc.Update(ctx, &vm); uerr != nil {
			slog.Error("migrate: report condition failed", "vm_id", vm.Meta.ID, "err", uerr)
		}
		return
	}

	oldHypervisor := vm.Status.Hypervisor
	newHypervisor, err := r.svc.scheduleMigration(ctx, vm.Spec, zone, oldHypervisor, vm.Status.MigrateTarget)
	if err != nil {
		vm.Status.Conditions = upsertCondition(vm.Status.Conditions, resource.Condition{
			Type:             "Unmigratable",
			Status:           resource.ConditionTrue,
			Reason:           "InsufficientCapacity",
			Message:          err.Error(),
			LastTransitionAt: time.Now(),
		})
		if _, uerr := r.svc.Update(ctx, &vm); uerr != nil {
			slog.Error("migrate: report condition failed", "vm_id", vm.Meta.ID, "err", uerr)
		}
		return
	}

	r.svc.releaseHypervisorCapacity(ctx, oldHypervisor, vm.Spec.VCPU, vm.Spec.MemoryMB)
	payload, _ := json.Marshal(DeleteCommand{VMID: vm.Meta.ID})
	msg := nats.NewMsg(CmdSubjectDelete(oldHypervisor))
	msg.Data = payload
	if _, err := r.js.PublishMsg(ctx, msg); err != nil {
		slog.Error("migrate: publish cleanup command to old hypervisor failed", "vm_id", vm.Meta.ID, "old_hypervisor", oldHypervisor, "err", err)
	}

	vm.Status.Hypervisor = newHypervisor
	vm.Status.MigrateTarget = ""
	vm.Status.Phase = PhaseScheduled
	vm.Status.Conditions = upsertCondition(vm.Status.Conditions, resource.Condition{
		Type:             "Unmigratable",
		Status:           resource.ConditionFalse,
		Reason:           "Scheduled",
		LastTransitionAt: time.Now(),
	})
	if _, err := r.svc.Update(ctx, &vm); err != nil {
		slog.Error("migrate: update failed", "vm_id", vm.Meta.ID, "err", err)
		r.svc.releaseHypervisorCapacity(ctx, newHypervisor, vm.Spec.VCPU, vm.Spec.MemoryMB)
	}
}

// publishStopCommand tells compute-agent to tear down the real VMM process;
// it reports back on EvtSubjectStopResult (handleStopResult) once it
// actually has, which is what advances this VM to Stopped -- see nats.go's
// StopCommand doc for why the jail/run directory (the root disk) is
// deliberately left alone here. Fire-and-forget: a dropped publish (or one
// that reaches a compute-agent that's since crashed) leaves the VM stuck in
// Stopping -- runStaleSweep below re-calls this for a VM that's been
// Stopping too long, same as it re-calls provisionAndPublish for a stuck
// Provisioning VM. Safe to call more than once for the same VM: handleStop
// always reports StopResult{Success:true} even when it finds nothing to
// stop (already gone, or never a real process at all), so a redundant
// resend just produces a redundant (harmless) StopResult.
func (r *Reconciler) publishStopCommand(ctx context.Context, vm VirtualMachine) {
	payload, _ := json.Marshal(StopCommand{VMID: vm.Meta.ID, Force: vm.Status.StopForce})
	msg := nats.NewMsg(CmdSubjectStop(vm.Status.Hypervisor))
	msg.Data = payload
	if _, err := r.js.PublishMsg(ctx, msg); err != nil {
		slog.Error("stop: publish stop command failed", "vm_id", vm.Meta.ID, "err", err)
	}
}

// provisionAndPublish creates (idempotently) this VM's NetworkInterfaces and
// VolumeAttachments and tells compute-agent to boot it, advancing the VM to
// Provisioning first. Shared by PhaseScheduled (a freshly-scheduled VM) and
// PhaseStarting (Service.Start on a previously-Stopped VM) -- see reconcile's
// case comment for why the exact same steps are correct for both.
func (r *Reconciler) provisionAndPublish(ctx context.Context, vm VirtualMachine) {
	// Create the NetworkInterface objects themselves now, not at Create
	// time: creating them earlier (e.g. while still Pending, possibly
	// never scheduled) would leak them with no owning VM ever having run.
	// createNetworkInterfaces names each with the deterministic
	// iface-<vm-id>-<index> convention (docs/architecture.md), so a
	// retry of this same reconcile (e.g. after the Update below fails)
	// re-creates nothing -- network's Create is idempotent by name.
	netifs, err := createNetworkInterfaces(ctx, r.svc.subnetClient, r.svc.netifClient, vm.Meta.TenantID, vm.Meta.ID, vm.Spec.NetworkInterfaces)
	if err != nil {
		slog.Error("provision: create network interfaces failed", "vm_id", vm.Meta.ID, "err", err)
		return
	}
	refs := make([]string, len(netifs))
	for i, n := range netifs {
		refs[i] = n.IfaceID
	}
	vm.Status.InterfaceRefs = refs

	// Same reasoning as NetworkInterfaces: VolumeAttachments are created
	// now, not at Create time, so a VM that's never actually scheduled
	// never leaves one behind with no owning VM. volInfos only carries
	// the ones that actually reached Attached (see createVolumeAttachments'
	// doc) -- this VM boots without whichever didn't, rather than being
	// blocked on them (attach-before-boot only, see docs/specs/volume.md).
	volInfos, volRefs, err := createVolumeAttachments(ctx, r.svc.volumeClient, r.svc.volumeAttachmentClient, vm.Meta.TenantID, vm.Meta.ID, vm.Spec.Volumes)
	if err != nil {
		slog.Error("provision: create volume attachments failed", "vm_id", vm.Meta.ID, "err", err)
		return
	}
	vm.Status.VolumeAttachmentRefs = volRefs

	vm.Status.Phase = PhaseProvisioning
	if _, err := r.svc.Update(ctx, &vm); err != nil {
		slog.Error("provision: update failed", "vm_id", vm.Meta.ID, "err", err)
		return
	}

	// This span is the root of its own trace, not a continuation of
	// whatever triggered the original Create() call: reconcile() runs off
	// an internal Watch loop, arbitrarily long after that call returned.
	// Its context is injected into the NATS message header so the
	// consuming compute-agent can link back to it (see
	// docs/specs/nats-messaging.md); vm_id is the correlation key that
	// actually lets this VM's whole lifecycle be found across the
	// resulting separate traces.
	ctx, span := tracer.Start(ctx, "compute.publish_create_command", trace.WithAttributes(
		attribute.String("vm_id", vm.Meta.ID),
		attribute.String("hypervisor", vm.Status.Hypervisor),
	))
	defer span.End()

	cmd := CreateCommand{
		VMID:       vm.Meta.ID,
		TenantID:   vm.Meta.TenantID,
		ImageID:    vm.Spec.ImageID,
		VCPU:       vm.Spec.VCPU,
		MemoryMB:   vm.Spec.MemoryMB,
		DriverHint: string(vm.Spec.DriverHint),
		Interfaces: netifs,
		UserData:   vm.Spec.UserData,
		Volumes:    volInfos,
	}
	// Resolve the Image to concrete boot inputs now (not at Create time:
	// the Image could have changed, and compute-agent has no image
	// service client of its own -- see nats.go's CreateCommand doc).
	// Create() already validated this Image exists/is Ready/matches
	// driver_hint, so a failure here is an unexpected race (e.g. the
	// Image was deleted between Create and this reconcile); leave the VM
	// in Provisioning and log rather than guess at a recovery.
	img, err := r.svc.imageClient.Get(ctx, &imagev1.GetImageRequest{TenantId: vm.Meta.TenantID, Id: vm.Spec.ImageID})
	if err != nil {
		span.RecordError(err)
		slog.Error("provision: resolve image failed", "vm_id", vm.Meta.ID, "image_id", vm.Spec.ImageID, "err", err)
		return
	}
	cmd.KernelURL = img.GetSpec().GetKernel().GetUrl()
	cmd.RootfsURL = img.GetSpec().GetRootfs().GetUrl()
	cmd.KernelDigest = img.GetSpec().GetKernel().GetDigest()
	cmd.RootfsDigest = img.GetSpec().GetRootfs().GetDigest()
	cmd.DiskURL = img.GetSpec().GetDisk().GetUrl()
	cmd.DiskDigest = img.GetSpec().GetDisk().GetDigest()
	cmd.BootArgs = img.GetSpec().GetBootArgs()

	payload, _ := json.Marshal(cmd)
	msg := nats.NewMsg(CmdSubjectCreate(vm.Status.Hypervisor))
	msg.Data = payload
	telemetry.InjectNATSHeader(ctx, msg.Header)
	if _, err := r.js.PublishMsg(ctx, msg); err != nil {
		span.RecordError(err)
		slog.Error("provision: publish create command failed", "vm_id", vm.Meta.ID, "err", err)
	}
}

// releaseIfReserved releases vm's Hypervisor capacity reservation on
// deletion, if it had ever reached far enough to hold one (Status.Hypervisor
// is only ever set by a successful schedule). A VM deleted while still
// Pending never held a reservation, so there is nothing to release.
//
// It also tells that Hypervisor's compute-agent to tear down whatever real
// process it may have started (fire-and-forget: VM deletion isn't gated on
// this, matching the rest of this system's "compute-agent state is
// best-effort, never authoritative" stance -- see docs/specs/vm-scheduling.md),
// and deletes every VolumeAttachment this VM ever created (also fire-and-
// forget) -- without this, a Volume this VM held would stay Attached
// forever, permanently blocking the exclusive-attach constraint
// (docs/architecture.md「具体的な排他制御」) against ever reusing it.
//
// The DeleteCommand is published before the VolumeAttachments are deleted
// (not after): deleting an attachment tears down its real iSCSI export
// (block-storage's UnexportVolume), and doing that before compute-agent
// has even been told to stop the VM would yank a still-running guest's
// disk out from under it. Publishing first at least gives compute-agent a
// head start on detaching on its own -- there's still no synchronization
// waiting for that to actually finish, same eventual-consistency tolerance
// as tap/cgroup cleanup already has elsewhere in this system.
func (r *Reconciler) releaseIfReserved(ctx context.Context, vm VirtualMachine) {
	if vm.Status.Hypervisor != "" {
		r.svc.releaseHypervisorCapacity(ctx, vm.Status.Hypervisor, vm.Spec.VCPU, vm.Spec.MemoryMB)

		payload, _ := json.Marshal(DeleteCommand{VMID: vm.Meta.ID})
		msg := nats.NewMsg(CmdSubjectDelete(vm.Status.Hypervisor))
		msg.Data = payload
		if _, err := r.js.PublishMsg(ctx, msg); err != nil {
			slog.Error("delete: publish delete command failed", "vm_id", vm.Meta.ID, "err", err)
		}
	}

	for _, attachmentID := range vm.Status.VolumeAttachmentRefs {
		if _, err := r.svc.volumeAttachmentClient.Delete(ctx, &blockstoragev1.DeleteVolumeAttachmentRequest{
			TenantId: vm.Meta.TenantID, Id: attachmentID,
		}); err != nil {
			slog.Error("delete: delete volume attachment failed", "vm_id", vm.Meta.ID, "attachment_id", attachmentID, "err", err)
		}
	}
}

// upsertCondition replaces the condition with the same Type if one exists,
// or appends a new one -- Conditions accumulate history by type rather than
// growing unboundedly on every repeated reconcile attempt.
func upsertCondition(conditions []resource.Condition, next resource.Condition) []resource.Condition {
	for i, c := range conditions {
		if c.Type == next.Type {
			conditions[i] = next
			return conditions
		}
	}
	return append(conditions, next)
}

// consumeResults handles compute-agent's vm.create-result events, advancing
// the VM to Running (or Error) once the agent reports back.
func (r *Reconciler) consumeResults(ctx context.Context) error {
	stream, err := r.js.Stream(ctx, evtStreamName)
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-reconciler-results",
		FilterSubject: "ms.compute.evt.*.vm.create-result",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	_, err = cons.Consume(func(msg jetstream.Msg) {
		defer msg.Ack()

		var res CreateResult
		if err := json.Unmarshal(msg.Data(), &res); err != nil {
			slog.Error("create-result: bad payload", "err", err)
			return
		}

		spanCtx, span := tracer.Start(ctx, "compute.handle_create_result",
			trace.WithLinks(telemetry.LinkFromNATSHeader(msg.Headers())),
			trace.WithAttributes(attribute.String("vm_id", res.VMID)),
		)
		defer span.End()
		r.handleCreateResult(spanCtx, res)
	})
	return err
}

func (r *Reconciler) handleCreateResult(ctx context.Context, res CreateResult) {
	// svc.Get is tenant-scoped; the result only carries the VM ID, so list
	// across tenants. Fine at this scale (see docs/architecture.md's target
	// scale); a real index would key by ID directly.
	vms, err := r.svc.List(ctx, "")
	if err != nil {
		slog.Error("create-result: list failed", "err", err)
		return
	}
	var vm *VirtualMachine
	for i := range vms {
		if vms[i].Meta.ID == res.VMID {
			vm = &vms[i]
			break
		}
	}
	if vm == nil || vm.Status.Phase != PhaseProvisioning {
		return // stale or unknown result; ignore
	}

	if res.Success {
		vm.Status.Phase = PhaseRunning
	} else {
		// Creation failed: the reservation this VM made at Scheduled time is
		// released now, since it will never actually run. Clearing Hypervisor
		// also marks the reservation as already released, so a later Delete
		// of this Error VM (releaseIfReserved) doesn't release it again.
		r.svc.releaseHypervisorCapacity(ctx, vm.Status.Hypervisor, vm.Spec.VCPU, vm.Spec.MemoryMB)
		vm.Status.Hypervisor = ""
		vm.Status.Phase = PhaseError
		vm.Status.Conditions = append(vm.Status.Conditions, resource.Condition{
			Type:             "CreateFailed",
			Status:           resource.ConditionTrue,
			Message:          res.Error,
			LastTransitionAt: time.Now(),
		})
	}
	if _, err := r.svc.Update(ctx, vm); err != nil {
		slog.Error("create-result: update failed", "vm_id", vm.Meta.ID, "err", err)
	}
}

// consumeStopResults handles compute-agent's vm.stop-result events,
// advancing the VM from Stopping to Stopped (or back to Running on failure)
// once the agent reports back -- mirrors consumeResults/handleCreateResult.
func (r *Reconciler) consumeStopResults(ctx context.Context) error {
	stream, err := r.js.Stream(ctx, evtStreamName)
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-reconciler-stop-results",
		FilterSubject: "ms.compute.evt.*.vm.stop-result",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	_, err = cons.Consume(func(msg jetstream.Msg) {
		defer msg.Ack()

		var res StopResult
		if err := json.Unmarshal(msg.Data(), &res); err != nil {
			slog.Error("stop-result: bad payload", "err", err)
			return
		}

		spanCtx, span := tracer.Start(ctx, "compute.handle_stop_result",
			trace.WithLinks(telemetry.LinkFromNATSHeader(msg.Headers())),
			trace.WithAttributes(attribute.String("vm_id", res.VMID)),
		)
		defer span.End()
		r.handleStopResult(spanCtx, res)
	})
	return err
}

func (r *Reconciler) handleStopResult(ctx context.Context, res StopResult) {
	vms, err := r.svc.List(ctx, "")
	if err != nil {
		slog.Error("stop-result: list failed", "err", err)
		return
	}
	var vm *VirtualMachine
	for i := range vms {
		if vms[i].Meta.ID == res.VMID {
			vm = &vms[i]
			break
		}
	}
	if vm == nil || vm.Status.Phase != PhaseStopping {
		return // stale or unknown result; ignore
	}

	if res.Success {
		vm.Status.Phase = PhaseStopped
	} else {
		// The VMM process is presumably still up (Stop never got to tear it
		// down); go back to Running rather than stranding the VM in
		// Stopping forever with no real handler left to advance it.
		vm.Status.Phase = PhaseRunning
		vm.Status.Conditions = upsertCondition(vm.Status.Conditions, resource.Condition{
			Type:             "StopFailed",
			Status:           resource.ConditionTrue,
			Message:          res.Error,
			LastTransitionAt: time.Now(),
		})
	}
	if _, err := r.svc.Update(ctx, vm); err != nil {
		slog.Error("stop-result: update failed", "vm_id", vm.Meta.ID, "err", err)
	}
}

func (r *Reconciler) subscribeHeartbeats() error {
	_, err := r.nc.Subscribe("ms.compute.evt.*.heartbeat", func(msg *nats.Msg) {
		var hb HeartbeatMsg
		if err := json.Unmarshal(msg.Data, &hb); err != nil {
			return
		}
		if err := r.svc.Heartbeat(context.Background(), hb.Hypervisor, hb.At); err != nil {
			slog.Warn("heartbeat: update failed", "hypervisor", hb.Hypervisor, "err", err)
		}
	})
	return err
}

// publishHypervisorStorageConnections republishes each Hypervisor's
// self-reported zone+storage_connections as a durable NATS event on every
// Added/Modified change, so block-storage can learn it without ever
// dialing compute's gRPC -- see nats.go's
// EvtSubjectHypervisorStorageConnections doc comment. Fires on every
// heartbeat-driven Modified event too, not just genuine storage_connections
// changes (Heartbeat() touches the same Hypervisor object) -- simpler than
// tracking last-published state, and cheap enough at this system's target
// scale (~500 hypervisors, docs/architecture.md「想定するユーザー像と
// スケール」) not to bother. Started once from Run; blocks until ctx is done.
func (r *Reconciler) publishHypervisorStorageConnections(ctx context.Context) {
	events, err := r.svc.WatchHypervisors(ctx, 0)
	if err != nil {
		slog.Error("watch hypervisors for storage-connections publish failed", "err", err)
		return
	}
	for e := range events {
		if e.Type != EventAdded && e.Type != EventModified {
			continue
		}
		h := e.Object
		names := make([]string, len(h.Status.StorageConnections))
		for i, c := range h.Status.StorageConnections {
			names[i] = c.Name
		}
		payload, _ := json.Marshal(HypervisorStorageConnectionsMsg{
			Hypervisor:         h.Meta.ID,
			Zone:               h.Status.Zone,
			StorageConnections: names,
		})
		msg := nats.NewMsg(EvtSubjectHypervisorStorageConnections(h.Meta.ID))
		msg.Data = payload
		if _, err := r.js.PublishMsg(ctx, msg); err != nil {
			slog.Warn("publish hypervisor storage-connections failed", "hypervisor", h.Meta.ID, "err", err)
		}
	}
}
