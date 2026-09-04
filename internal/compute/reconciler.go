package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

// Reconciler drives VirtualMachines from Pending through Provisioning by
// talking to compute-agent over NATS. Scheduling is a stub (round-robin over
// a fixed node list) until the real Node inventory and scheduler exist; the
// point of this pass is to validate the async command/event shape end to end.
type Reconciler struct {
	svc   *Service
	nc    *nats.Conn
	js    jetstream.JetStream
	nodes []string

	mu       sync.Mutex
	nextNode int
	seen     map[string]time.Time // node -> last heartbeat, informational only for now
}

func NewReconciler(svc *Service, nc *nats.Conn, js jetstream.JetStream, nodes []string) *Reconciler {
	return &Reconciler{
		svc:   svc,
		nc:    nc,
		js:    js,
		nodes: nodes,
		seen:  make(map[string]time.Time),
	}
}

// Run blocks, reconciling VMs and heartbeats until ctx is done.
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

	events, err := r.svc.Watch(ctx, "", 0) // all tenants: internal use only
	if err != nil {
		return fmt.Errorf("watch vms: %w", err)
	}
	for e := range events {
		if e.Type != EventAdded && e.Type != EventModified {
			continue
		}
		r.reconcile(ctx, e.VM)
	}
	return nil
}

func (r *Reconciler) reconcile(ctx context.Context, vm VirtualMachine) {
	switch vm.Status.Phase {
	case PhasePending:
		node, ok := r.pickNode()
		if !ok {
			slog.Warn("unschedulable: no nodes available", "vm_id", vm.Meta.ID)
			return
		}
		vm.Status.Phase = PhaseScheduled
		vm.Status.Node = node
		if _, err := r.svc.Update(ctx, &vm); err != nil {
			slog.Error("schedule: update failed", "vm_id", vm.Meta.ID, "err", err)
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
		if _, err := r.js.Publish(ctx, CmdSubjectCreate(vm.Status.Node), payload); err != nil {
			slog.Error("provision: publish create command failed", "vm_id", vm.Meta.ID, "err", err)
		}
	}
}

func (r *Reconciler) pickNode() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.nodes) == 0 {
		return "", false
	}
	node := r.nodes[r.nextNode%len(r.nodes)]
	r.nextNode++
	return node, true
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
		r.mu.Lock()
		r.seen[hb.Node] = hb.At
		r.mu.Unlock()
	})
	return err
}
