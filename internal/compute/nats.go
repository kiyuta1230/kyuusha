package compute

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Subject naming follows docs/architecture.md's "NATS JetStream:
// subject/stream設計" section: ms.<service>.<cmd|evt>.<hypervisor>.<resource-type>.<verb>

// CmdSubjectCreate etc. are shared by the compute service (Reconciler) and
// compute-agent, which is why they live here rather than unexported.

func CmdSubjectCreate(hypervisor string) string {
	return fmt.Sprintf("ms.compute.cmd.%s.vm.create", hypervisor)
}

func CmdSubjectDelete(hypervisor string) string {
	return fmt.Sprintf("ms.compute.cmd.%s.vm.delete", hypervisor)
}

func CmdSubjectStop(hypervisor string) string {
	return fmt.Sprintf("ms.compute.cmd.%s.vm.stop", hypervisor)
}

// CmdSubjectHotplug is where a live Resize/AttachVolume/DetachVolume's
// cloud-hypervisor api-socket call travels (see liveops.go). Unlike
// ConsoleRequestSubject, this DOES sit under ms.compute.cmd.> (captured by
// COMPUTE_CMD, same at-least-once/ack-on-receipt durability contract as
// StopCommand): a live hotplug command must not be silently dropped if
// compute-agent is briefly unreachable. Its reply, unlike StopResult, does
// NOT travel via COMPUTE_EVT -- see HotplugResult.
func CmdSubjectHotplug(hypervisor string) string {
	return fmt.Sprintf("ms.compute.cmd.%s.vm.hotplug", hypervisor)
}

func EvtSubjectCreateResult(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.vm.create-result", hypervisor)
}

func EvtSubjectStopResult(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.vm.stop-result", hypervisor)
}

func EvtSubjectHeartbeat(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.heartbeat", hypervisor)
}

// EvtSubjectHypervisorStorageConnections is published by the Reconciler
// (see reconciler.go's publishHypervisorStorageConnections) whenever a
// Hypervisor registers/re-registers, carrying its self-reported zone +
// storage_connections. block-storage subscribes to this directly (wildcard
// hypervisor) to learn, without ever dialing compute's gRPC (which would
// make today's one-way compute->block-storage dependency bidirectional --
// see docs/specs/volume.md「検証フロー」), which zones each declared
// storage_connection name is actually backed by a Hypervisor in.
func EvtSubjectHypervisorStorageConnections(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.storage-connections", hypervisor)
}

// HypervisorStorageConnectionsMsg is the payload of
// EvtSubjectHypervisorStorageConnections. Only connection names travel here
// (not local_path -- block-storage only needs to know *that* a zone can
// reach a named connection, not compute-agent's own local mount point for
// it).
type HypervisorStorageConnectionsMsg struct {
	Hypervisor         string   `json:"hypervisor"`
	Zone               string   `json:"zone"`
	StorageConnections []string `json:"storage_connections,omitempty"`
}

// ConsoleRequestSubject is deliberately NOT under `ms.compute.cmd.>`: console
// access is ephemeral/live, not a durable work-queue command, so it must not
// be captured (and retained) by the COMPUTE_CMD JetStream stream. Published
// and subscribed via plain NATS core (see console.go).
func ConsoleRequestSubject(hypervisor string) string {
	return fmt.Sprintf("ms.compute.console.%s.request", hypervisor)
}

const (
	cmdStreamName = "COMPUTE_CMD"
	evtStreamName = "COMPUTE_EVT"
)

// CreateCommand is published to cmdSubjectCreate(hypervisor) when a VM enters
// Provisioning. compute-agent acks on receipt, not on completion (see
// docs/architecture.md), and reports back on evtSubjectCreateResult.
//
// DriverHint/KernelURL/RootfsURL/DiskURL/BootArgs are the VM's
// already-validated Image resolved to concrete boot inputs at publish time
// (see reconciler.go's PhaseScheduled branch): compute-agent has no image
// service client of its own, so everything it needs to actually boot the
// VM (via whichever of internal/compute-agent/fcvmm or .../chvmm
// driver_hint selects) travels in this one message. Exactly one of
// (KernelURL+RootfsURL) or DiskURL is non-empty, matching the Image's own
// format (KERNEL_ROOTFS or QCOW2 -- see docs/specs/image.md); an Image
// whose format no registered driver consumes leaves all three empty, in
// which case compute-agent stub-succeeds as before.
type CreateCommand struct {
	VMID       string `json:"vm_id"`
	TenantID   string `json:"tenant_id"`
	ImageID    string `json:"image_id"`
	VCPU       int32  `json:"vcpu"`
	MemoryMB   int64  `json:"memory_mb"`
	DriverHint string `json:"driver_hint"`
	KernelURL  string `json:"kernel_url,omitempty"`
	RootfsURL  string `json:"rootfs_url,omitempty"`
	// KernelDigest/RootfsDigest are the Image's spec.kernel.digest/
	// spec.rootfs.digest verbatim ("sha256:<hex>", see docs/specs/
	// image.md) -- compute-agent's internal/compute-agent/imagestore
	// verifies the downloaded bytes against these before caching them.
	// Empty for an Image that never had one set, same as KernelURL/
	// RootfsURL being empty for a QCOW2 Image.
	KernelDigest string `json:"kernel_digest,omitempty"`
	RootfsDigest string `json:"rootfs_digest,omitempty"`
	// DiskURL/DiskDigest are spec.disk.url/spec.disk.digest for a QCOW2
	// Image -- see internal/compute-agent/vmm.BootSpec's identical fields
	// for why this is never set alongside KernelURL/RootfsURL. Only
	// internal/compute-agent/chvmm consumes it (QCOW2 requires
	// driver_hint=CLOUD_HYPERVISOR, enforced at Create time by
	// internal/compute/image.go).
	DiskURL    string `json:"disk_url,omitempty"`
	DiskDigest string `json:"disk_digest,omitempty"`
	BootArgs   string `json:"boot_args,omitempty"`
	// Interfaces is populated by reconciler.go's PhaseScheduled branch from
	// the NetworkInterfaces it just created (see internal/compute/
	// network.go's createNetworkInterfaces) -- same reasoning as
	// KernelURL/RootfsURL above: compute-agent has no network service
	// client of its own, so everything it needs to wire a real tap device
	// per interface (internal/compute-agent/netsetup) travels here.
	Interfaces []NetworkInterfaceInfo `json:"interfaces,omitempty"`
	// UserData is VirtualMachineSpec.user_data verbatim (see
	// docs/architecture.md "UserData注入: NoCloud seed disk"); empty means
	// don't inject anything. compute-agent builds a cloud-init NoCloud
	// seed disk from this plus Interfaces (for network-config) --
	// see internal/compute-agent/vmm/seed.go.
	UserData string `json:"user_data,omitempty"`
	// Volumes is populated by reconciler.go's PhaseScheduled branch from
	// the VolumeAttachments it just created (see
	// internal/compute/volume.go's createVolumeAttachments) -- same
	// reasoning as Interfaces: compute-agent has no block-storage service
	// client of its own, so what internal/compute-agent/volumeref needs to
	// find the already-visible device/file on this host (kyuusha never
	// logs in, mounts, or exports anything -- see docs/architecture.md
	// 「block-storageのバックエンド抽象化」) travels here.
	Volumes []VolumeAttachInfo `json:"volumes,omitempty"`
	// PciDevices is vm.Status.AllocatedPciDevices verbatim -- the specific
	// PCI addresses (e.g. "0000:3b:00.0") scheduleVM/scheduleConstraints
	// already reserved against this Hypervisor's self-reported
	// available_devices (see hypervisor_service.go's reservePciDevices).
	// compute-agent never picks which device to use itself, only passes
	// each address straight to cloud-hypervisor's --device flag (fcvmm
	// never sees this: CLOUD_HYPERVISOR-only, enforced at Create time --
	// see docs/architecture.md「PCIデバイス(GPU等)パススルー」).
	PciDevices []string `json:"pci_devices,omitempty"`
	// NumaNode is vm.Status.AllocatedNumaNode verbatim -- UnpinnedNumaNode
	// (-1) if spec.numa_pinned is false. Both fcvmm and chvmm consume this
	// (unlike PciDevices, not restricted to either driver) via
	// internal/compute-agent/cgroup's cpuset pinning.
	NumaNode int32 `json:"numa_node"`
}

// VolumeAttachInfo is one VM Volume attachment: the protocol/connection/
// identifier straight off the Volume's own spec (see
// internal/compute/volume.go's createVolumeAttachments), letting
// internal/compute-agent/volumeref find the already-visible device/file on
// the target host without kyuusha itself ever logging in, mounting, or
// exporting anything.
type VolumeAttachInfo struct {
	AttachmentID      string `json:"attachment_id"`
	Protocol          string `json:"protocol"`
	StorageConnection string `json:"storage_connection"`
	Identifier        string `json:"identifier"`
	// SizeGB is the Volume's own spec.size_gb, self-reported and
	// unverifiable at Create time (kyuusha never provisions -- see
	// docs/architecture.md「block-storageのバックエンド抽象化」). Carried this far only
	// so compute-agent can log a warning if the real size it observes at
	// boot time diverges from it (see docs/open-questions.md「Volumeの
	// 申告内容...」) -- not used for anything else.
	SizeGB int64 `json:"size_gb,omitempty"`
}

// NetworkInterfaceInfo is one VM network attachment, already resolved to
// concrete wiring inputs: IPAddress/MACAddress come from the
// NetworkInterface itself, CIDR/GatewayIP/VLANID from its Subnet. IPAddress
// and CIDR are both empty if that NetworkInterface's own IP allocation
// hadn't succeeded yet by the time the VM was scheduled (its Subnet's pool
// was exhausted -- see docs/specs/network.md's IPAM section); compute-agent
// skips wiring that one NIC rather than blocking the whole VM's boot on it.
type NetworkInterfaceInfo struct {
	IfaceID    string `json:"iface_id"`
	IPAddress  string `json:"ip_address,omitempty"`
	MACAddress string `json:"mac_address,omitempty"`
	CIDR       string `json:"cidr,omitempty"`
	GatewayIP  string `json:"gateway_ip,omitempty"`
	VLANID     int32  `json:"vlan_id,omitempty"`
	// Primary mirrors the originating NetworkAttachment.Primary: only the
	// primary interface gets a default route in the guest (see
	// docker/fc-guest-init.sh) -- a VM with several NICs would otherwise
	// end up with an ambiguous or last-one-wins default gateway.
	Primary bool `json:"primary,omitempty"`
}

type CreateResult struct {
	VMID    string `json:"vm_id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// DeleteCommand is published (fire-and-forget -- no result event) when a VM
// with a live Hypervisor assignment is deleted, so compute-agent can tear
// down whatever real process it may have started for it. Safe to receive
// for a VM compute-agent never actually booted (e.g. it stub-succeeded, or
// never reached Provisioning): Stop is a no-op in that case.
type DeleteCommand struct {
	VMID string `json:"vm_id"`
}

// StopCommand is published to CmdSubjectStop(hypervisor) when a VM enters
// Stopping (see Service.Stop/reconciler.go's PhaseStopping case).
// compute-agent stops the real VMM process (SIGTERM+grace, or immediately on
// Force) but deliberately does NOT remove the jail/run directory backing
// this VM's root disk -- that only happens at real Delete, via each VMM
// driver's Destroy (see internal/compute-agent/vmm.VMM) -- so the disk
// survives for a later Start to reuse. Unlike DeleteCommand, this carries a
// result event (EvtSubjectStopResult) back, since Service.Stop needs to know
// when Stopping actually finished to advance the VM to Stopped.
type StopCommand struct {
	VMID  string `json:"vm_id"`
	Force bool   `json:"force"`
}

type StopResult struct {
	VMID    string `json:"vm_id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// HotplugOp discriminates HotplugCommand's operation. One struct with a
// discriminator rather than three separate command types -- mirrors
// CreateCommand's own "several optional field groups, only the relevant one
// populated per case" convention, and keeps compute-agent's wiring to one
// durable consumer/one handler instead of three.
type HotplugOp string

const (
	HotplugOpResize       HotplugOp = "RESIZE"
	HotplugOpAddDisk      HotplugOp = "ADD_DISK"
	HotplugOpRemoveDevice HotplugOp = "REMOVE_DEVICE"
)

// HotplugCommand is published to CmdSubjectHotplug(hypervisor) by
// Reconciler.LiveResize/LiveAttachVolume/LiveDetachVolume (liveops.go) for a
// Running+CLOUD_HYPERVISOR VM. Carries its own ReplySubject (a fresh
// nc.NewInbox()), exactly like ConsoleRequest: the result comes back over
// that inbox via plain core NATS (HotplugResult), not COMPUTE_EVT, since
// only the one blocked RPC caller ever cares about it.
type HotplugCommand struct {
	VMID         string    `json:"vm_id"`
	TenantID     string    `json:"tenant_id"`
	DriverHint   string    `json:"driver_hint"`
	Op           HotplugOp `json:"op"`
	ReplySubject string    `json:"reply_subject"`

	// VCPU/MemoryMB are set when Op == HotplugOpResize.
	VCPU     int32 `json:"vcpu,omitempty"`
	MemoryMB int64 `json:"memory_mb,omitempty"`

	// DeviceID is set when Op == HotplugOpAddDisk / HotplugOpRemoveDevice:
	// the caller-chosen id passed straight through to cloud-hypervisor's
	// DiskConfig.id / VmRemoveDevice.id -- always the owning
	// VolumeAttachment's own Meta.ID (see liveops.go), so a later
	// REMOVE_DEVICE never needs a separately-persisted "which
	// cloud-hypervisor id did ADD_DISK return" mapping.
	DeviceID string `json:"device_id,omitempty"`

	// Protocol/StorageConnection/Identifier/SizeGB are set when Op ==
	// HotplugOpAddDisk: compute-agent resolves the real device path itself
	// via volumeref.Resolve, same as CreateCommand.Volumes -- compute never
	// learns a real device path.
	Protocol          string `json:"protocol,omitempty"`
	StorageConnection string `json:"storage_connection,omitempty"`
	Identifier        string `json:"identifier,omitempty"`
	SizeGB            int64  `json:"size_gb,omitempty"`
}

// HotplugResult is published to a HotplugCommand's own ReplySubject (plain
// core NATS, not COMPUTE_EVT) once compute-agent has attempted the
// operation.
type HotplugResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type HeartbeatMsg struct {
	Hypervisor string    `json:"hypervisor"`
	At         time.Time `json:"at"`
}

// ConsoleRequest is published to ConsoleRequestSubject(hypervisor) when a
// client wants to read or follow a VM's serial console (see
// docs/specs/firecracker-boot.md and console.go). compute-agent tails its
// local console log for VMID and publishes raw byte chunks to ReplySubject:
// an immediate zero-length message first (so the caller's bounded wait for
// a first response succeeds even when there's nothing new to say yet, e.g.
// an already-quiet Follow session), then TailBytes worth of history, then
// -- if Follow -- new output as it's written. A message carrying
// ConsoleDoneHeader marks the end (with ConsoleErrorHeader set if it ended
// because of an error, e.g. no console for this VM). Follow sessions stop
// when a message arrives on ReplySubject+".stop", or after a safety timeout.
type ConsoleRequest struct {
	VMID         string `json:"vm_id"`
	TailBytes    int64  `json:"tail_bytes"`
	Follow       bool   `json:"follow"`
	ReplySubject string `json:"reply_subject"`
}

const (
	ConsoleDoneHeader  = "Kyuusha-Console-Done"
	ConsoleErrorHeader = "Kyuusha-Console-Error"
)

// EnsureStreams creates COMPUTE_CMD/COMPUTE_EVT if they don't already exist.
// Safe to call from both compute and compute-agent at startup.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cmdStreamName,
		Subjects:  []string{"ms.compute.cmd.>"},
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		return fmt.Errorf("ensure %s stream: %w", cmdStreamName, err)
	}

	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      evtStreamName,
		Subjects:  []string{"ms.compute.evt.>"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("ensure %s stream: %w", evtStreamName, err)
	}
	return nil
}
