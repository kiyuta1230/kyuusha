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

import (
	"context"
	"log/slog"
)

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
	// Volumes are already-created VolumeAttachments to attach as extra
	// virtio-blk drives before boot (see internal/compute-agent/volumeref
	// and docs/specs/volume.md) -- resolved the same "everything needed
	// travels in the boot command, no separate service client" way as
	// NetworkInterfaces. Attach-before-boot only: there is no hot-plug path
	// for a Volume requested after the VM is already Running.
	Volumes []VolumeAttachInfo
}

// VolumeAttachInfo is one already-resolved Volume attachment: the
// protocol/connection/identifier a Volume's spec carries, letting
// internal/compute-agent/volumeref find the already-visible device/file on
// this host without kyuusha itself ever logging in, mounting, or
// exporting anything (see docs/architecture.md「訂正: 責務の境界を...」).
// agent.go builds these from compute.CreateCommand.Volumes.
type VolumeAttachInfo struct {
	AttachmentID      string
	Protocol          string
	StorageConnection string
	Identifier        string
	// SizeGB is the Volume's own spec.size_gb, self-reported and
	// unverifiable at Create time (kyuusha never provisions -- see
	// docs/architecture.md「訂正: 責務の境界を...」). Used only by
	// WarnIfSizeMismatch below, the one point where a real size is ever
	// actually observed.
	SizeGB int64
}

// bytesPerGB assumes the binary (1024-based) convention most storage
// tooling (ZFS, LVM) itself uses for a "G"-suffixed size, since that's
// almost certainly where a real size_gb value would have come from.
const bytesPerGB = 1 << 30

// sizeMismatchTolerance is how far observedBytes may diverge from a
// Volume's declared size_gb (as a fraction of the declared size) before
// WarnIfSizeMismatch logs anything -- loose enough to absorb ordinary
// backend rounding/overhead (a ZFS zvol's usable size rarely matches its
// nominal size exactly), tight enough to still catch a size_gb that's
// wildly wrong.
const sizeMismatchTolerance = 0.10

// WarnIfSizeMismatch logs (does not fail the boot) when a Volume's real,
// just-observed size diverges from what its spec declared by more than
// sizeMismatchTolerance. size_gb is entirely self-reported -- kyuusha
// never provisions a Volume, so it has no way to verify this at Create
// time (see docs/open-questions.md「Volumeの申告内容（存在確認・サイズ）が
// 一切検証されない」) -- this boot-time observation is the only point in
// the whole system where a real size is ever seen at all, so it's the
// only place a mismatch can be caught, and only after the fact.
func WarnIfSizeMismatch(v VolumeAttachInfo, observedBytes int64) {
	if v.SizeGB <= 0 {
		return
	}
	declaredBytes := v.SizeGB * bytesPerGB
	diff := observedBytes - declaredBytes
	if diff < 0 {
		diff = -diff
	}
	if float64(diff) <= float64(declaredBytes)*sizeMismatchTolerance {
		return
	}
	slog.Warn("vmm: volume's real size does not match its declared size_gb -- size_gb is self-reported and unverifiable at Create time, see docs/open-questions.md",
		"attachment_id", v.AttachmentID, "declared_bytes", declaredBytes, "observed_bytes", observedBytes)
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
