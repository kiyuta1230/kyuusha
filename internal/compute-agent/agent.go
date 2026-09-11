// Package computeagent is the compute-agent side of the NATS command/event
// exchange described in docs/architecture.md. It dispatches each
// CreateCommand to whichever VMM driver (internal/compute-agent/vmm.VMM)
// is registered under its driver_hint -- see docs/specs/firecracker-boot.md
// (FIRECRACKER) and docs/specs/qemu-boot.md (QEMU). A driver_hint with no
// registered driver, or one missing its artifact URLs (shouldn't happen
// given compute's own validation, but handled defensively), stub-succeeds
// as before this package had any real VMM integration.
package computeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent/vmm"
	"gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent/volumeref"

	blockstorage "gitlab.com/ki.yuta1230/kyuusha/internal/block-storage"
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
	Hypervisors computev1.HypervisorServiceClient
	// BootstrapToken authorizes self-registration into a zone
	// (internal/bootstraptoken) -- the resulting Hypervisor's zone comes
	// from this token, not from any locally-configured value, since the
	// agent's own claim about its zone isn't trusted.
	BootstrapToken      string
	AllocatableVCPU     int32
	AllocatableMemoryMB int64
	SupportedDrivers    []string
	// StorageConnections declares which storage connections this host
	// already has established (an iSCSI/NVMe-oF session already logged in,
	// or an NFS export already mounted -- see internal/compute-agent/
	// volumeref and docs/architecture.md「訂正: 責務の境界を...」), sent at
	// self-registration so compute can eventually use it as a scheduling
	// constraint (not yet implemented -- see docs/open-questions.md).
	// cmd/compute-agent/main.go builds this from the same -storage-
	// connections flag value it also uses to build the local
	// volumeref.Connections map each VMM driver resolves Volumes against.
	StorageConnections []*computev1.StorageConnection
	// LocalStorageConnections is the same declared set as
	// StorageConnections, in the shape internal/compute-agent/volumeref
	// needs (also handed to each VMM driver, see cmd/compute-agent/main.go)
	// -- used here to answer block-storage's verify-volume command
	// (handleVerifyVolume) the exact same way a real VM boot would resolve
	// a Volume, just without attaching it to anything.
	LocalStorageConnections volumeref.Connections

	// Drivers boots/tears down VMs, keyed by driver_hint (e.g.
	// string(compute.VmmDriverFirecracker), string(compute.VmmDriverQEMU)).
	// cmd/compute-agent/main.go always populates both in production;
	// handleCreate falls back to the old stub behavior for a driver_hint
	// with no entry (or a nil map), so tests that don't set this keep
	// working.
	Drivers map[string]vmm.VMM
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

	// block-storage's verify-volume command (internal/block-storage/
	// verification.go) -- a separate stream block-storage owns/creates,
	// not compute's own COMPUTE_CMD, so EnsureStreams here too (idempotent,
	// same as compute.EnsureStreams above; either service may start first
	// under docker-compose).
	if err := blockstorage.EnsureStreams(ctx, a.JS); err != nil {
		return err
	}
	verifyStream, err := a.JS.Stream(ctx, "BLOCKSTORAGE_CMD")
	if err != nil {
		return err
	}
	verifyCons, err := verifyStream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-agent-" + a.Hypervisor + "-verify-volume",
		FilterSubject: blockstorage.CmdSubjectVerifyVolume(a.Hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	verifyConsumeCtx, err := verifyCons.Consume(a.handleVerifyVolume)
	if err != nil {
		return err
	}
	defer verifyConsumeCtx.Stop()

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
		BootstrapToken:      a.BootstrapToken,
		AllocatableVcpu:     a.AllocatableVCPU,
		AllocatableMemoryMb: a.AllocatableMemoryMB,
		SupportedDrivers:    a.SupportedDrivers,
		StorageConnections:  a.StorageConnections,
	}
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		h, err := a.Hypervisors.Register(ctx, req)
		if err == nil {
			slog.Info("compute-agent: registered", "hypervisor", a.Hypervisor, "zone", h.GetStatus().GetZone())
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
// the work is durably accepted, not once it completes). A command whose
// driver_hint has a registered Drivers entry, and both artifact URLs set,
// is booted for real; everything else (an unregistered driver_hint, or a
// command missing URLs -- shouldn't happen given compute's validation, but
// handled defensively) stub-succeeds as before.
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
	if driver, ok := a.Drivers[cmd.DriverHint]; ok && driver != nil && cmd.KernelURL != "" && cmd.RootfsURL != "" {
		slog.Info("compute-agent: booting VM", "vm_id", cmd.VMID, "hypervisor", a.Hypervisor, "driver_hint", cmd.DriverHint, "interfaces", len(cmd.Interfaces))
		if err := driver.Boot(ctx, vmm.BootSpec{
			VMID:              cmd.VMID,
			VCPU:              cmd.VCPU,
			MemoryMB:          cmd.MemoryMB,
			KernelURL:         cmd.KernelURL,
			RootfsURL:         cmd.RootfsURL,
			BootArgs:          cmd.BootArgs,
			NetworkInterfaces: buildNetIfaces(cmd.VMID, cmd.Interfaces),
			UserData:          cmd.UserData,
			Volumes:           buildVolumeInfos(cmd.Volumes),
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

// handleDelete tears down whatever real process may have booted for this
// VM. Every registered driver's Stop is tried: exactly one of them (if
// any) actually booted a given VM, since a VM's Image format determines
// which drivers can even consume it (see internal/compute/image.go's
// validateImage), and each driver's Stop is already documented as a no-op
// for a vm_id it never booted, so trying them all is safe and needs no
// separate bookkeeping of which driver "owns" a VM. Fire-and-forget: no
// result event, see reconciler.go's releaseIfReserved.
func (a *Agent) handleDelete(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.DeleteCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad delete command", "err", err)
		return
	}
	for _, driver := range a.Drivers {
		driver.Stop(cmd.VMID)
	}
}

// handleVerifyVolume answers block-storage's request to confirm a Volume's
// identifier actually exists on this host, and its real size -- see
// internal/block-storage/verification.go. Uses the exact same
// internal/compute-agent/volumeref.Resolve a real VM boot would, just
// without attaching anything to a VM: this is pure discovery, so there's
// nothing to place into a jail or clean up afterward.
func (a *Agent) handleVerifyVolume(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd blockstorage.VerifyVolumeCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad verify-volume command", "err", err)
		return
	}

	result := blockstorage.VerifyVolumeResult{TenantID: cmd.TenantID, VolumeID: cmd.VolumeID}
	_, sizeBytes, err := volumeref.Resolve(a.LocalStorageConnections, cmd.Protocol, cmd.StorageConnection, cmd.Identifier)
	if err != nil {
		result.Error = err.Error()
	} else {
		result.Success = true
		result.SizeBytes = sizeBytes
	}

	payload, _ := json.Marshal(result)
	resultMsg := nats.NewMsg(blockstorage.EvtSubjectVerifyVolumeResult(a.Hypervisor))
	resultMsg.Data = payload
	if _, err := a.JS.PublishMsg(context.Background(), resultMsg); err != nil {
		slog.Error("compute-agent: publish verify-volume result failed", "volume_id", cmd.VolumeID, "err", err)
	}
}

// buildNetIfaces resolves cmd.Interfaces (compute.NetworkInterfaceInfo,
// see nats.go) into what a VMM driver's Boot needs to actually wire a tap
// device for each: mainly parsing CIDR down to a prefix length, since the
// kernel cmdline convention each driver builds (kyuusha.net.<i>.ip=<ip>/
// <prefix>) needs it in that form. An interface with no IPAddress/CIDR (its
// NetworkInterface's IP allocation hadn't succeeded yet when the VM was
// scheduled -- see docs/specs/network.md) is skipped rather than failing
// the whole boot: the guest simply comes up with one fewer NIC than
// requested.
func buildNetIfaces(vmID string, infos []compute.NetworkInterfaceInfo) []vmm.NetIface {
	var out []vmm.NetIface
	for _, ni := range infos {
		if ni.IPAddress == "" || ni.CIDR == "" {
			slog.Warn("compute-agent: skipping network interface with no allocated IP yet", "vm_id", vmID, "iface_id", ni.IfaceID)
			continue
		}
		_, ipnet, err := net.ParseCIDR(ni.CIDR)
		if err != nil {
			slog.Warn("compute-agent: skipping network interface with unparseable cidr", "vm_id", vmID, "iface_id", ni.IfaceID, "cidr", ni.CIDR, "err", err)
			continue
		}
		prefixLen, _ := ipnet.Mask.Size()
		out = append(out, vmm.NetIface{
			IfaceID:    ni.IfaceID,
			MACAddress: ni.MACAddress,
			IPAddress:  ni.IPAddress,
			PrefixLen:  prefixLen,
			GatewayIP:  ni.GatewayIP,
			VLANID:     ni.VLANID,
			Primary:    ni.Primary,
		})
	}
	return out
}

// buildVolumeInfos resolves cmd.Volumes (compute.VolumeAttachInfo, see
// nats.go) into what a VMM driver's Boot needs to find each Volume's
// already-visible device/file on this host: a straight field-for-field
// copy -- see internal/compute-agent/volumeref, which is where the actual
// discovery happens (at Boot time, inside each driver), not here.
func buildVolumeInfos(infos []compute.VolumeAttachInfo) []vmm.VolumeAttachInfo {
	out := make([]vmm.VolumeAttachInfo, len(infos))
	for i, v := range infos {
		out[i] = vmm.VolumeAttachInfo{
			AttachmentID:      v.AttachmentID,
			Protocol:          v.Protocol,
			StorageConnection: v.StorageConnection,
			Identifier:        v.Identifier,
			SizeGB:            v.SizeGB,
		}
	}
	return out
}

func (a *Agent) publishHeartbeat() {
	hb := compute.HeartbeatMsg{Hypervisor: a.Hypervisor, At: time.Now()}
	payload, _ := json.Marshal(hb)
	if err := a.NC.Publish(compute.EvtSubjectHeartbeat(a.Hypervisor), payload); err != nil {
		slog.Error("compute-agent: publish heartbeat failed", "err", err)
	}
}
