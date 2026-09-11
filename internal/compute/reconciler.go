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

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	imagev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

var tracer = otel.Tracer("gitlab.com/ki.yuta1230/kyuusha/internal/compute")

// pendingSweepInterval implements docs/architecture.md's "Pendingのまま...
// 報告し続け" retry: reconcile() only runs off VM Watch events, so a VM that
// failed to schedule (no Hypervisor had room) would otherwise never be
// retried once capacity frees up elsewhere, since freeing capacity is a
// Hypervisor change, not a VM change, and doesn't appear on this Watch.
const pendingSweepInterval = 10 * time.Second

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
	if err := r.subscribeHeartbeats(); err != nil {
		return fmt.Errorf("subscribe heartbeats: %w", err)
	}
	go r.svc.runHealthSweep(ctx)
	go r.runPendingSweep(ctx)
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

func (r *Reconciler) runPendingSweep(ctx context.Context) {
	ticker := time.NewTicker(pendingSweepInterval)
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
			for _, vm := range vms {
				if vm.Status.Phase == PhasePending {
					r.reconcile(ctx, vm)
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

		hypervisorID, err := r.svc.scheduleVM(ctx, vm.Spec, zone)
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

	case PhaseScheduled:
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
