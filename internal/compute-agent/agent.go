// Package computeagent is the compute-agent side of the NATS command/event
// exchange described in docs/architecture.md. It dispatches each
// CreateCommand to whichever VMM driver (internal/compute-agent/vmm.VMM)
// is registered under its driver_hint -- see docs/specs/firecracker-boot.md
// (FIRECRACKER) and docs/specs/cloud-hypervisor-boot.md (CLOUD_HYPERVISOR).
// A driver_hint with no
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
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/volumeref"

	blockstorage "github.com/kiyuta1230/kyuusha/internal/block-storage"
	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

var tracer = otel.Tracer("github.com/kiyuta1230/kyuusha/internal/compute-agent")

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
	// volumeref and docs/architecture.md「block-storageのバックエンド抽象化」), sent at
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

	// AvailableDevices declares which PCI devices this host has already
	// bound to vfio-pci and made available for passthrough (see
	// docs/architecture.md "PCIデバイス(GPU等)パススルー") -- sent at self-
	// registration so compute can reserve them against a VM's
	// spec.pci_devices (internal/compute/hypervisor_service.go's
	// reservePciDevices). Allocated is ignored by the server on input; see
	// hypervisor.proto's RegisterHypervisorRequest.available_devices.
	// cmd/compute-agent/main.go builds this from the -pci-devices flag,
	// after verifying each address is actually vfio-pci-bound.
	AvailableDevices []*computev1.PciDevice
	// NumaNodes declares this host's NUMA topology (node id, host logical
	// CPUs, total memory) -- sent at self-registration so compute can
	// schedule a spec.numa_pinned VM onto a node with enough spare
	// vcpu/memory_mb (internal/compute/hypervisor_service.go's
	// reserveNumaNode). allocated_vcpu/allocated_memory_mb are ignored by
	// the server on input, same as PciDevice.allocated -- see
	// hypervisor.proto's RegisterHypervisorRequest.numa_nodes.
	// cmd/compute-agent/main.go builds this from /sys/devices/system/node
	// (detectNumaTopology), unconditionally (no flag: unlike pci_devices,
	// there's nothing to opt into -- it's just facts about the host).
	NumaNodes []*computev1.NumaNode

	// MigrationRegistry is the OCI registry host[:port] this compute-agent
	// pushes a VM's current root disk to (Migrate(transfer_root_disk=true),
	// see internal/compute-agent/imagestore.PushOCIBlob) and deletes
	// temporary migration artifacts from afterward. Empty (the default)
	// means this host can't serve either half of that feature -- a PUSH
	// command then fails with an explicit error (surfaced as a failed
	// Migrate, never a silent fallback to the lossy re-clone-from-Image
	// path) rather than being silently accepted and doing nothing.
	MigrationRegistry string
	// MigrationRegistryRef, if set, is what gets embedded in a pushed
	// artifact's URL instead of MigrationRegistry -- same
	// push-via-one-address/reference-a-different-one split as
	// `kyuusha image build`'s -registry/-registry-ref (see cmd/compute-agent/
	// main.go). Empty means same as MigrationRegistry.
	MigrationRegistryRef string
	// MigrationRegistryPlainHTTP pushes/deletes over plain HTTP instead of
	// HTTPS -- same meaning as `kyuusha image build`'s -plain-http.
	MigrationRegistryPlainHTTP bool

	// Drivers boots/tears down VMs, keyed by driver_hint (e.g.
	// string(compute.VmmDriverFirecracker), string(compute.VmmDriverCloudHypervisor)).
	// cmd/compute-agent/main.go always populates both in production;
	// handleCreate falls back to the old stub behavior for a driver_hint
	// with no entry (or a nil map), so tests that don't set this keep
	// working.
	Drivers map[string]vmm.VMM

	// lastAppliedACL tracks, per iface_id, the resource_version of the
	// last network.UpdateACLCommand this Agent actually applied (see
	// handleUpdateACL) -- guards against a redelivered or reordered stale
	// command re-applying an older rule set over a newer one already in
	// effect. Lazily initialized; empty (including after a restart -- this
	// is never persisted) just means the next command for that iface_id is
	// treated as newer than anything seen before, which is always correct
	// since every command carries the interface's *complete* current rule
	// set, never a delta (see network.UpdateACLCommand's own doc comment).
	lastAppliedACLMu sync.Mutex
	lastAppliedACL   map[string]int64
}

// reconciler is implemented by any vmm.VMM driver that persists enough
// state to adopt a still-running VM across a compute-agent restart (see
// fcvmm.Manager.Reconcile/chvmm.Manager.Reconcile and vmm.BootRecord) --
// not part of vmm.VMM itself since it's a one-time startup step, not
// something agent.go ever dispatches by driver_hint.
type reconciler interface {
	Reconcile()
}

// Run reconciles every driver's still-running VMs from a previous process
// (see the reconciler interface -- must happen before registering/
// consuming any commands, so a resent Create/Stop for a VM this process
// forgot about finds it already adopted, not silently missing), registers
// with compute (retrying briefly in case it isn't up yet -- e.g. at
// docker-compose startup), and then blocks, processing create commands and
// publishing heartbeats until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	for _, driver := range a.Drivers {
		if r, ok := driver.(reconciler); ok {
			r.Reconcile()
		}
	}

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

	stopCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-agent-" + a.Hypervisor + "-stop",
		FilterSubject: compute.CmdSubjectStop(a.Hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	stopConsumeCtx, err := stopCons.Consume(a.handleStop)
	if err != nil {
		return err
	}
	defer stopConsumeCtx.Stop()

	hotplugCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-agent-" + a.Hypervisor + "-hotplug",
		FilterSubject: compute.CmdSubjectHotplug(a.Hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	hotplugConsumeCtx, err := hotplugCons.Consume(a.handleHotplug)
	if err != nil {
		return err
	}
	defer hotplugConsumeCtx.Stop()

	migrateArtifactCons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-agent-" + a.Hypervisor + "-migrate-artifact",
		FilterSubject: compute.CmdSubjectMigrateArtifact(a.Hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	migrateArtifactConsumeCtx, err := migrateArtifactCons.Consume(a.handleMigrateArtifact)
	if err != nil {
		return err
	}
	defer migrateArtifactConsumeCtx.Stop()

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

	// network's UpdateFirewallRules-triggered ACL update -- same "a
	// separate stream the other service owns/creates" shape as
	// BLOCKSTORAGE_CMD above.
	if err := network.EnsureStreams(ctx, a.JS); err != nil {
		return err
	}
	networkStream, err := a.JS.Stream(ctx, "NETWORK_CMD")
	if err != nil {
		return err
	}
	updateACLCons, err := networkStream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "compute-agent-" + a.Hypervisor + "-update-acl",
		FilterSubject: network.CmdSubjectUpdateACL(a.Hypervisor),
		AckPolicy:     jetstream.AckExplicitPolicy,
		// MaxDeliver bounds retries for a NetworkInterface whose VM hasn't
		// finished booting on this host yet (see handleUpdateACL: it
		// deliberately doesn't Ack until some driver reports the tap as
		// wired) -- 20 x the default 30s AckWait is a generous ~10 minutes,
		// comfortably past any real VM boot time, without retrying forever
		// for a VM that will genuinely never boot here (deleted before
		// ever booting on this host, or scheduled to a different one by
		// the time this message was published).
		MaxDeliver: 20,
	})
	if err != nil {
		return err
	}
	updateACLConsumeCtx, err := updateACLCons.Consume(a.handleUpdateACL)
	if err != nil {
		return err
	}
	defer updateACLConsumeCtx.Stop()

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
		AvailableDevices:    a.AvailableDevices,
		NumaNodes:           a.NumaNodes,
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
	hasBootableImage := (cmd.KernelURL != "" && cmd.RootfsURL != "") || cmd.DiskURL != ""
	if driver, ok := a.Drivers[cmd.DriverHint]; ok && driver != nil && hasBootableImage {
		slog.Info("compute-agent: booting VM", "vm_id", cmd.VMID, "hypervisor", a.Hypervisor, "driver_hint", cmd.DriverHint, "interfaces", len(cmd.Interfaces))
		attached, err := driver.Boot(ctx, vmm.BootSpec{
			VMID:              cmd.VMID,
			TenantID:          cmd.TenantID,
			VCPU:              cmd.VCPU,
			MemoryMB:          cmd.MemoryMB,
			KernelURL:         cmd.KernelURL,
			RootfsURL:         cmd.RootfsURL,
			KernelDigest:      cmd.KernelDigest,
			RootfsDigest:      cmd.RootfsDigest,
			DiskURL:           cmd.DiskURL,
			DiskDigest:        cmd.DiskDigest,
			BootArgs:          cmd.BootArgs,
			NetworkInterfaces: buildNetIfaces(cmd.VMID, cmd.Interfaces),
			UserData:          cmd.UserData,
			Volumes:           buildVolumeInfos(cmd.TenantID, cmd.Volumes),
			PciDevices:        cmd.PciDevices,
			NumaNode:          cmd.NumaNode,
		})
		if err != nil {
			span.RecordError(err)
			slog.Error("compute-agent: boot failed", "vm_id", cmd.VMID, "err", err)
			result = compute.CreateResult{VMID: cmd.VMID, Success: false, Error: err.Error()}
		} else {
			a.reportAttachedVolumes(ctx, attached)
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
// VM, AND removes its jail/run directory (its root disk included) -- unlike
// handleStop, this VM is never coming back, so its disk should not linger
// either (see docs/architecture.md's VM lifecycle). Every registered
// driver's Destroy is tried: exactly one of them (if any) actually booted a
// given VM, since a VM's Image format determines which drivers can even
// consume it (see internal/compute/image.go's validateImage), and each
// driver's Destroy is already documented as a no-op for a vm_id it never
// booted, so trying them all is safe and needs no separate bookkeeping of
// which driver "owns" a VM. Fire-and-forget: no result event, see
// reconciler.go's releaseIfReserved.
func (a *Agent) handleDelete(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.DeleteCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad delete command", "err", err)
		return
	}
	for _, driver := range a.Drivers {
		driver.Destroy(cmd.VMID)
	}
}

// handleStop tears down whatever real process may have booted for this VM,
// WITHOUT removing its jail/run directory (its root disk survives -- see
// docs/architecture.md's VM lifecycle and handleDelete's contrasting
// comment). Every registered driver's Stop is tried, same reasoning as
// handleDelete trying every Destroy. Unlike handleDelete, this reports back
// on EvtSubjectStopResult: Service.Stop's Stopping->Stopped transition
// (reconciler.go's handleStopResult) needs to know once the process is
// actually gone, not merely signaled -- each driver's Stop already blocks
// until that's true.
func (a *Agent) handleStop(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.StopCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad stop command", "err", err)
		return
	}

	ctx, span := tracer.Start(context.Background(), "compute-agent.handle_stop",
		trace.WithLinks(telemetry.LinkFromNATSHeader(msg.Headers())),
		trace.WithAttributes(attribute.String("vm_id", cmd.VMID), attribute.String("hypervisor", a.Hypervisor)),
	)
	defer span.End()

	for _, driver := range a.Drivers {
		driver.Stop(cmd.VMID, cmd.Force)
	}

	result := compute.StopResult{VMID: cmd.VMID, Success: true}
	payload, _ := json.Marshal(result)
	resultMsg := nats.NewMsg(compute.EvtSubjectStopResult(a.Hypervisor))
	resultMsg.Data = payload
	telemetry.InjectNATSHeader(ctx, resultMsg.Header)
	if _, err := a.JS.PublishMsg(ctx, resultMsg); err != nil {
		span.RecordError(err)
		slog.Error("compute-agent: publish stop-result failed", "vm_id", cmd.VMID, "err", err)
	}
}

// handleUpdateACL re-applies a NetworkInterface's current ingress_rules/
// egress_rules (see internal/compute-agent/snap) once the VM they belong
// to has actually finished booting on this host. Unlike every other
// handleX above, this deliberately does NOT Ack on receipt: only once some
// driver reports the tap as wired (ApplyACL's applied=true) does it Ack --
// otherwise JetStream's own redelivery (bounded by the consumer's
// MaxDeliver, see Run) is the retry mechanism for "this VM hasn't finished
// booting yet", with no separate polling loop needed here.
func (a *Agent) handleUpdateACL(msg jetstream.Msg) {
	var cmd network.UpdateACLCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad update_acl command", "err", err)
		_ = msg.Ack() // malformed payload will never parse differently on redelivery
		return
	}

	if !a.shouldApplyACL(cmd.IfaceID, cmd.ResourceVersion) {
		_ = msg.Ack() // a newer command for this interface already applied
		return
	}

	update := vmm.ACLUpdate{
		IfaceID: cmd.IfaceID, SubnetID: cmd.SubnetID, SubnetCIDR: cmd.SubnetCIDR, GatewayIP: cmd.GatewayIP,
		IPAddress: cmd.IPAddress, MACAddress: cmd.MACAddress,
		IngressRules: toVMMFirewallRulesFromNetwork(cmd.IngressRules),
		EgressRules:  toVMMFirewallRulesFromNetwork(cmd.EgressRules),
	}
	for _, driver := range a.Drivers {
		applied, err := driver.ApplyACL(cmd.VMID, update)
		if !applied {
			continue
		}
		if err != nil {
			slog.Error("compute-agent: apply ACL failed", "vm_id", cmd.VMID, "iface_id", cmd.IfaceID, "err", err)
		} else {
			a.recordAppliedACL(cmd.IfaceID, cmd.ResourceVersion)
		}
		_ = msg.Ack() // a driver had this tap: either applied, or genuinely failed -- neither is "not ready yet"
		return
	}
	// No driver has this tap wired yet -- leave unacked for redelivery.
}

func (a *Agent) shouldApplyACL(ifaceID string, resourceVersion int64) bool {
	a.lastAppliedACLMu.Lock()
	defer a.lastAppliedACLMu.Unlock()
	return resourceVersion > a.lastAppliedACL[ifaceID]
}

func (a *Agent) recordAppliedACL(ifaceID string, resourceVersion int64) {
	a.lastAppliedACLMu.Lock()
	defer a.lastAppliedACLMu.Unlock()
	if a.lastAppliedACL == nil {
		a.lastAppliedACL = make(map[string]int64)
	}
	a.lastAppliedACL[ifaceID] = resourceVersion
}

// toVMMFirewallRulesFromNetwork mirrors toVMMFirewallRules (defined below,
// for compute.FirewallRuleInfo) but for network.FirewallRuleInfo -- a
// separate wire type from a separate service, same shape, not unified into
// one conversion since the two source types aren't related by anything
// other than coincidence.
func toVMMFirewallRulesFromNetwork(rules []network.FirewallRuleInfo) []vmm.FirewallRule {
	var out []vmm.FirewallRule
	for _, r := range rules {
		out = append(out, vmm.FirewallRule{Protocol: r.Protocol, PortRange: r.PortRange, SourceCIDR: r.SourceCIDR, Action: r.Action})
	}
	return out
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
			IfaceID:      ni.IfaceID,
			SubnetID:     ni.SubnetID,
			Zone:         ni.Zone,
			MACAddress:   ni.MACAddress,
			IPAddress:    ni.IPAddress,
			PrefixLen:    prefixLen,
			GatewayIP:    ni.GatewayIP,
			VLANID:       ni.VLANID,
			Primary:      ni.Primary,
			SubnetCIDR:   ni.CIDR,
			IngressRules: toVMMFirewallRules(ni.IngressRules),
			EgressRules:  toVMMFirewallRules(ni.EgressRules),
		})
	}
	return out
}

func toVMMFirewallRules(rules []compute.FirewallRuleInfo) []vmm.FirewallRule {
	var out []vmm.FirewallRule
	for _, r := range rules {
		out = append(out, vmm.FirewallRule{Protocol: r.Protocol, PortRange: r.PortRange, SourceCIDR: r.SourceCIDR, Action: r.Action})
	}
	return out
}

// buildVolumeInfos resolves cmd.Volumes (compute.VolumeAttachInfo, see
// nats.go) into what a VMM driver's Boot needs to find each Volume's
// already-visible device/file on this host: a straight field-for-field
// copy -- see internal/compute-agent/volumeref, which is where the actual
// discovery happens (at Boot time, inside each driver), not here.
func buildVolumeInfos(tenantID string, infos []compute.VolumeAttachInfo) []vmm.VolumeAttachInfo {
	out := make([]vmm.VolumeAttachInfo, len(infos))
	for i, v := range infos {
		out[i] = vmm.VolumeAttachInfo{
			AttachmentID:      v.AttachmentID,
			TenantID:          tenantID,
			Protocol:          v.Protocol,
			StorageConnection: v.StorageConnection,
			Identifier:        v.Identifier,
			SizeGB:            v.SizeGB,
		}
	}
	return out
}

// reportAttachedVolumes tells block-storage where each Volume actually
// landed (device_path) and on which Hypervisor -- see
// docs/specs/volume.md "status.device_path/status.hypervisor", previously
// always empty for lack of exactly this report-back path. Fire-and-forget,
// same as the heartbeat: a dropped publish just means that one
// VolumeAttachment's status stays unreported until the next VM using it
// boots (there is no periodic retry, unlike the Volume verification flow,
// since this only ever fires at an actual successful boot, not on a timer).
func (a *Agent) reportAttachedVolumes(ctx context.Context, attached []vmm.AttachedVolume) {
	for _, v := range attached {
		payload, err := json.Marshal(blockstorage.VolumeAttachedEvent{
			AttachmentID: v.AttachmentID,
			TenantID:     v.TenantID,
			Hypervisor:   a.Hypervisor,
			DevicePath:   v.DevicePath,
		})
		if err != nil {
			continue
		}
		msg := nats.NewMsg(blockstorage.EvtSubjectVolumeAttached(a.Hypervisor))
		msg.Data = payload
		if _, err := a.JS.PublishMsg(ctx, msg); err != nil {
			slog.Warn("compute-agent: publish volume-attached event failed", "attachment_id", v.AttachmentID, "err", err)
		}
	}
}

func (a *Agent) publishHeartbeat() {
	hb := compute.HeartbeatMsg{Hypervisor: a.Hypervisor, At: time.Now()}
	payload, _ := json.Marshal(hb)
	if err := a.NC.Publish(compute.EvtSubjectHeartbeat(a.Hypervisor), payload); err != nil {
		slog.Error("compute-agent: publish heartbeat failed", "err", err)
	}
}
