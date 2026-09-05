// Package computeagent is the compute-agent side of the NATS command/event
// exchange described in docs/architecture.md. It deliberately stubs out the
// actual VMM work (Firecracker/QEMU) for now: every create command
// "succeeds" instantly. The point of this pass is the NATS plumbing, not the
// hypervisor integration.
package computeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"gitlab.com/ki.yuta1230/kyuusha/internal/compute"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

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
// the work is durably accepted, not once it completes) and then reports
// success immediately, since there is no real VMM call yet.
func (a *Agent) handleCreate(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.CreateCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad create command", "err", err)
		return
	}

	slog.Info("compute-agent: stub-creating VM", "vm_id", cmd.VMID, "hypervisor", a.Hypervisor)
	result := compute.CreateResult{VMID: cmd.VMID, Success: true}
	payload, _ := json.Marshal(result)
	if _, err := a.JS.Publish(context.Background(), compute.EvtSubjectCreateResult(a.Hypervisor), payload); err != nil {
		slog.Error("compute-agent: publish create-result failed", "vm_id", cmd.VMID, "err", err)
	}
}

func (a *Agent) publishHeartbeat() {
	hb := compute.HeartbeatMsg{Hypervisor: a.Hypervisor, At: time.Now()}
	payload, _ := json.Marshal(hb)
	if err := a.NC.Publish(compute.EvtSubjectHeartbeat(a.Hypervisor), payload); err != nil {
		slog.Error("compute-agent: publish heartbeat failed", "err", err)
	}
}
