package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

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

	events, err := r.svc.Watch(ctx, "", 0) // all tenants: internal use only
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
		hypervisorID, err := r.svc.scheduleVM(ctx, vm.Spec)
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
		vm.Status.Phase = PhaseProvisioning
		if _, err := r.svc.Update(ctx, &vm); err != nil {
			slog.Error("provision: update failed", "vm_id", vm.Meta.ID, "err", err)
			return
		}
		cmd := CreateCommand{
			VMID:     vm.Meta.ID,
			TenantID: vm.Meta.TenantID,
			ImageID:  vm.Spec.ImageID,
			VCPU:     vm.Spec.VCPU,
			MemoryMB: vm.Spec.MemoryMB,
		}
		payload, _ := json.Marshal(cmd)
		if _, err := r.js.Publish(ctx, CmdSubjectCreate(vm.Status.Hypervisor), payload); err != nil {
			slog.Error("provision: publish create command failed", "vm_id", vm.Meta.ID, "err", err)
		}
	}
}

// releaseIfReserved releases vm's Hypervisor capacity reservation on
// deletion, if it had ever reached far enough to hold one (Status.Hypervisor
// is only ever set by a successful schedule). A VM deleted while still
// Pending never held a reservation, so there is nothing to release.
func (r *Reconciler) releaseIfReserved(ctx context.Context, vm VirtualMachine) {
	if vm.Status.Hypervisor == "" {
		return
	}
	r.svc.releaseHypervisorCapacity(ctx, vm.Status.Hypervisor, vm.Spec.VCPU, vm.Spec.MemoryMB)
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
		r.handleCreateResult(ctx, res)
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
