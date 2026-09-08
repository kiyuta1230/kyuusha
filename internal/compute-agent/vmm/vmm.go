// Package vmm is the driver-agnostic contract compute-agent dispatches
// through: fcvmm.Manager (driver_hint=FIRECRACKER) and qemuvmm.Manager
// (driver_hint=QEMU) both implement VMM against the same BootSpec/NetIface
// shapes, so agent.go picks one by cmd.DriverHint instead of hardcoding a
// single VMM. Both existing drivers boot from the same KERNEL_ROOTFS Image
// assets (kernel + raw rootfs, no bootloader) -- see docs/specs/
// firecracker-boot.md and docs/specs/qemu-boot.md. BootSpec deliberately
// carries only what that boot method needs today; a future driver booting
// from a self-contained disk image instead (see docs/architecture.md
// "Firecracker/QEMU実装メモ" on QCOW2 -- not implemented, not currently
// planned) would most likely need its own additional fields here, or its
// own BootSpec-like type, rather than forcing every existing driver to
// carry fields it can't use.
package vmm

import "context"

// VMM is what compute-agent needs from any VMM driver implementation.
type VMM interface {
	// Boot fetches (or reuses cached copies of) spec's kernel/rootfs, gives
	// the VM its own writable rootfs copy, and starts the VMM process
	// against them. It returns once the process has either exited
	// immediately (an error) or stayed up past a driver-specific grace
	// period (success) -- success does NOT mean the guest kernel finished
	// booting, only that the VMM process itself launched.
	Boot(ctx context.Context, spec BootSpec) error
	// Stop tears down vmID's VMM process if this driver has one running. A
	// no-op if this driver never booted a real process for vmID (a stub-
	// succeeded VM booted by a different driver, or one that already
	// exited).
	Stop(vmID string)
	// ConsoleLogPath is where Boot(vmID's spec) captures the VMM process's
	// stdout/stderr (== the guest's serial console, ttyS0). Exists only
	// once Boot has actually run for this vmID.
	ConsoleLogPath(vmID string) string
}

// BootSpec is what a VMM driver needs to boot one VM. Callers (agent.go)
// build this from a compute.CreateCommand.
type BootSpec struct {
	VMID              string
	VCPU              int32
	MemoryMB          int64
	KernelURL         string
	RootfsURL         string
	BootArgs          string
	NetworkInterfaces []NetIface
	// UserData is spec.user_data verbatim (see docs/architecture.md
	// "UserData注入: NoCloud seed disk"); empty means don't inject
	// anything, matching that field's own doc comment. Non-empty triggers
	// building a cidata-labeled seed disk (see seed.go) carrying it plus
	// meta-data and, if any NetworkInterfaces have an allocated IP,
	// network-config.
	UserData string
}

// NetIface is one already-resolved network attachment Boot should wire for
// real (see internal/compute-agent/netsetup): agent.go builds these from
// compute.CreateCommand.Interfaces, having already parsed CIDR down to
// PrefixLen. IPAddress/CIDR are only ever missing (and so never turned into
// a NetIface at all) when the NetworkInterface's own IP allocation hadn't
// succeeded yet at Scheduled time -- see docs/specs/network.md.
type NetIface struct {
	IfaceID    string
	MACAddress string
	IPAddress  string
	PrefixLen  int
	GatewayIP  string
	VLANID     int32
	Primary    bool
}
