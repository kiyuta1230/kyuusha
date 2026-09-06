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

func EvtSubjectCreateResult(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.vm.create-result", hypervisor)
}

func EvtSubjectHeartbeat(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.heartbeat", hypervisor)
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
// DriverHint/KernelURL/RootfsURL/BootArgs are the VM's already-validated
// Image resolved to concrete boot inputs at publish time (see
// reconciler.go's PhaseScheduled branch): compute-agent has no image
// service client of its own, so everything it needs to actually boot a
// driver_hint=FIRECRACKER VM travels in this one message. KernelURL/
// RootfsURL are empty for QCOW2 images (QEMU driver remains unimplemented,
// see docs/specs/firecracker-boot.md) -- compute-agent stub-succeeds that
// case as before.
type CreateCommand struct {
	VMID       string `json:"vm_id"`
	TenantID   string `json:"tenant_id"`
	ImageID    string `json:"image_id"`
	VCPU       int32  `json:"vcpu"`
	MemoryMB   int64  `json:"memory_mb"`
	DriverHint string `json:"driver_hint"`
	KernelURL  string `json:"kernel_url,omitempty"`
	RootfsURL  string `json:"rootfs_url,omitempty"`
	BootArgs   string `json:"boot_args,omitempty"`
	// Interfaces is populated by reconciler.go's PhaseScheduled branch from
	// the NetworkInterfaces it just created (see internal/compute/
	// network.go's createNetworkInterfaces) -- same reasoning as
	// KernelURL/RootfsURL above: compute-agent has no network service
	// client of its own, so everything it needs to wire a real tap device
	// per interface (internal/compute-agent/netsetup) travels here.
	Interfaces []NetworkInterfaceInfo `json:"interfaces,omitempty"`
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
