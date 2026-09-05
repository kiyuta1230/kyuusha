// Package computeagent is the compute-agent side of the NATS command/event
// exchange described in docs/architecture.md. driver_hint=FIRECRACKER VMs
// are booted for real via fcvmm (see docs/specs/firecracker-boot.md for
// what that milestone does and doesn't cover); QEMU remains a stub, since
// that driver has no implementation at all yet.
package computeagent

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

	computeagentfc "gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent/fcvmm"

	"gitlab.com/ki.yuta1230/kyuusha/internal/compute"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

var tracer = otel.Tracer("gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent")

type Agent struct {
	Hypervisor        string
	NC                *nats.Conn
	JS                jetstream.JetStream
	HeartbeatInterval time.Duration

	// Hypervisors is compute's HypervisorService, dialed directly by
	// cmd/compute-agent/main.go (bypassing api-gateway: this is east-west
	// traffic -- see docs/architecture.md "Hypervisor自己登録とzone割当"). Used
	// once at startup to self-register/re-register.
	Hypervisors         computev1.HypervisorServiceClient
	Zone                string
	AllocatableVCPU     int32
	AllocatableMemoryMB int64
	SupportedDrivers    []string

	// Firecracker boots/tears down driver_hint=FIRECRACKER VMs. Never nil in
	// practice (cmd/compute-agent/main.go always constructs one), but
	// handleCreate falls back to the old stub behavior if it is, so tests
	// that don't set it keep working.
	Firecracker *computeagentfc.Manager
}

// Run registers with compute (retrying briefly in case it isn't up yet --
// e.g. at docker-compose startup) and then blocks, processing create
// commands and publishing heartbeats until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.register(ctx); err != nil {
		return fmt.Errorf("register hypervisor: %w", err)
	}

	if err := compute.EnsureStreams(ctx, a.JS); err != nil {
		return err
	}

	stream, err := a.JS.Stream(ctx, "COMPUTE_CMD")
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-agent-" + a.Hypervisor,
		FilterSubject: compute.CmdSubjectCreate(a.Hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	consumeCtx, err := cons.Consume(a.handleCreate)
	if err != nil {
		return err
	}
	defer consumeCtx.Stop()

	deleteCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-agent-" + a.Hypervisor + "-delete",
		FilterSubject: compute.CmdSubjectDelete(a.Hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	deleteConsumeCtx, err := deleteCons.Consume(a.handleDelete)
	if err != nil {
		return err
	}
	defer deleteConsumeCtx.Stop()

	// Plain NATS core subscription, not JetStream: console access is
	// ephemeral/live, not a durable work-queue command -- see
	// compute.ConsoleRequestSubject.
	consoleSub, err := a.NC.Subscribe(compute.ConsoleRequestSubject(a.Hypervisor), a.handleConsoleRequest)
	if err != nil {
		return err
	}
	defer consoleSub.Unsubscribe()

	interval := a.HeartbeatInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			a.publishHeartbeat()
		}
	}
}

// register self-registers with compute's HypervisorService (idempotent --
// see RegisterHypervisorRequest), retrying for a while since compute may not
// have finished starting yet at the same moment this agent has.
func (a *Agent) register(ctx context.Context) error {
	req := &computev1.RegisterHypervisorRequest{
		Hypervisor:          a.Hypervisor,
		Zone:                a.Zone,
		AllocatableVcpu:     a.AllocatableVCPU,
		AllocatableMemoryMb: a.AllocatableMemoryMB,
		SupportedDrivers:    a.SupportedDrivers,
	}
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		_, err := a.Hypervisors.Register(ctx, req)
		if err == nil {
			slog.Info("compute-agent: registered", "hypervisor", a.Hypervisor, "zone", a.Zone)
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return lastErr
}

// handleCreate acks on receipt (per docs/architecture.md: agents ack once
// the work is durably accepted, not once it completes). driver_hint=
// FIRECRACKER commands with both artifact URLs set are booted for real via
// fcvmm; everything else (QEMU, or a Firecracker command missing URLs --
// shouldn't happen given compute's validation, but handled defensively)
// stub-succeeds as before.
func (a *Agent) handleCreate(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.CreateCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad create command", "err", err)
		return
	}

	// Linked to (not a child of) whatever span published this command --
	// see docs/specs/nats-messaging.md: this hop's producer and consumer are
	// separated by an unpredictable queueing delay.
	ctx, span := tracer.Start(context.Background(), "compute-agent.handle_create",
		trace.WithLinks(telemetry.LinkFromNATSHeader(msg.Headers())),
		trace.WithAttributes(attribute.String("vm_id", cmd.VMID), attribute.String("hypervisor", a.Hypervisor)),
	)
	defer span.End()

	result := compute.CreateResult{VMID: cmd.VMID, Success: true}
	if a.Firecracker != nil && cmd.DriverHint == string(compute.VmmDriverFirecracker) && cmd.KernelURL != "" && cmd.RootfsURL != "" {
		slog.Info("compute-agent: booting VM", "vm_id", cmd.VMID, "hypervisor", a.Hypervisor)
		if err := a.Firecracker.Boot(ctx, computeagentfc.BootSpec{
			VMID:      cmd.VMID,
			VCPU:      cmd.VCPU,
			MemoryMB:  cmd.MemoryMB,
			KernelURL: cmd.KernelURL,
			RootfsURL: cmd.RootfsURL,
			BootArgs:  cmd.BootArgs,
		}); err != nil {
			span.RecordError(err)
			slog.Error("compute-agent: boot failed", "vm_id", cmd.VMID, "err", err)
			result = compute.CreateResult{VMID: cmd.VMID, Success: false, Error: err.Error()}
		}
	} else {
		slog.Info("compute-agent: stub-creating VM", "vm_id", cmd.VMID, "hypervisor", a.Hypervisor, "driver_hint", cmd.DriverHint)
	}

	payload, _ := json.Marshal(result)
	resultMsg := nats.NewMsg(compute.EvtSubjectCreateResult(a.Hypervisor))
	resultMsg.Data = payload
	telemetry.InjectNATSHeader(ctx, resultMsg.Header)
	if _, err := a.JS.PublishMsg(ctx, resultMsg); err != nil {
		span.RecordError(err)
		slog.Error("compute-agent: publish create-result failed", "vm_id", cmd.VMID, "err", err)
	}
}

// handleDelete tears down whatever real process Firecracker may have
// booted for this VM. Fire-and-forget: no result event, see
// reconciler.go's releaseIfReserved.
func (a *Agent) handleDelete(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.DeleteCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad delete command", "err", err)
		return
	}
	if a.Firecracker != nil {
		a.Firecracker.Stop(cmd.VMID)
	}
}

func (a *Agent) publishHeartbeat() {
	hb := compute.HeartbeatMsg{Hypervisor: a.Hypervisor, At: time.Now()}
	payload, _ := json.Marshal(hb)
	if err := a.NC.Publish(compute.EvtSubjectHeartbeat(a.Hypervisor), payload); err != nil {
		slog.Error("compute-agent: publish heartbeat failed", "err", err)
	}
}
