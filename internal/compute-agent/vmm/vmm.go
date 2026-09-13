// Package vmm is the driver-agnostic contract compute-agent dispatches
// through: fcvmm.Manager (driver_hint=FIRECRACKER) and chvmm.Manager
// (driver_hint=CLOUD_HYPERVISOR) both implement VMM against the same
// BootSpec/NetIface shapes, so agent.go picks one by cmd.DriverHint instead
// of hardcoding a single VMM. Both existing drivers boot from the same
// KERNEL_ROOTFS Image assets (kernel + raw rootfs, no bootloader) -- see
// docs/specs/firecracker-boot.md and docs/specs/cloud-hypervisor-boot.md.
// BootSpec deliberately carries only what that boot method needs today; a
// future driver booting from a self-contained disk image instead (see
// docs/architecture.md's VMM driver notes on QCOW2 -- not implemented, not
// currently planned) would most likely need its own additional fields
// here, or its own BootSpec-like type, rather than forcing every existing
// driver to carry fields it can't use.
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
	// booting, only that the VMM process itself launched. On success, the
	// returned []AttachedVolume covers every spec.Volumes entry that was
	// actually resolved and wired in (agent.go reports these to block-
	// storage -- see docs/specs/volume.md "status.device_path/
	// status.hypervisor"); nil on any error, since nothing was attached for
	// certain.
	Boot(ctx context.Context, spec BootSpec) ([]AttachedVolume, error)
	// Stop tears down vmID's VMM process if this driver has one running,
	// blocking until it has actually exited (force=false sends SIGTERM,
	// waits a driver-specific grace period, then SIGKILLs if still up;
	// force=true SIGKILLs immediately) -- unlike Boot, callers need this
	// synchronous, since Service.Stop's Stopping->Stopped transition (see
	// docs/architecture.md's VM lifecycle) is only correct once the process
	// is actually gone, not merely signaled. A no-op if this driver never
	// booted a real process for vmID (a stub-succeeded VM booted by a
	// different driver, or one that already exited). Does NOT remove the
	// jail/run directory backing the VM's root disk -- see Destroy.
	Stop(vmID string, force bool)
	// Destroy stops vmID (as Stop, force) if still running and then removes
	// the jail/run directory entirely, including its root disk -- the real
	// teardown Delete needs (see docs/architecture.md's VM lifecycle: a
	// Stop/Start cycle must not lose the disk, but Delete must). A no-op
	// (returns immediately, nothing to remove) if this driver never booted a
	// real process for vmID.
	Destroy(vmID string)
	// ConsoleLogPath is where Boot(vmID's spec) captures the VMM process's
	// stdout/stderr (== the guest's serial console, ttyS0). Exists only
	// once Boot has actually run for this vmID.
	ConsoleLogPath(vmID string) string
	// Running lists every VM this driver currently has booted -- freshly,
	// or adopted via Reconcile from a previous compute-agent process (see
	// vmm.BootRecord). The only consumer is /metrics/resources' collector
	// (internal/compute-agent/resourcemetrics, see docs/architecture.md
	// 「払い出したリソース自身のメトリクス」): it needs to know which vm_ids
	// have a live cgroup to read stats from, and which tenant_id to label
	// them with, without duplicating either driver's own bookkeeping.
	Running() []RunningVM
}

// RunningVM is one VM a VMM driver reports via Running.
type RunningVM struct {
	VMID     string
	TenantID string
	// PID is this VM's real VMM process id, for /metrics/resources' VM-disk
	// collector (internal/compute-agent/procio) -- unlike cgroup-based
	// CPU/memory stats, per-process I/O accounting works identically
	// regardless of a Volume's backend (NFS included), so it's the only
	// disk metric that's universal across driver_hint/protocol.
	PID int
	// NetworkInterfaces are the NetworkInterface ids (not tap names) wired
	// for this VM, in the same order Boot wired them -- the collector
	// derives each one's tap name itself via netsetup.TapName, since that
	// mapping is a pure function and doesn't need storing twice.
	NetworkInterfaces []string
	// Volumes are this VM's resolved attachments (see AttachedVolume) --
	// the collector reads per-device block stats for whichever of these
	// resolve to a real block device (ISCSI/NVME_OF); an NFS-backed
	// DevicePath (a regular file) has no per-file host-side I/O counter and
	// is silently skipped, same spirit as cgroup stats being best-effort.
	Volumes []AttachedVolume
}

// BootSpec is what a VMM driver needs to boot one VM. Callers (agent.go)
// build this from a compute.CreateCommand.
type BootSpec struct {
	VMID string
	// TenantID is the VM's own owning tenant (not to be confused with
	// VolumeAttachInfo.TenantID below, which happens to always be the same
	// value here since a VolumeAttachment lives in its VM's tenant, but
	// exists for that struct's own, narrower reason). Carried only for
	// Running's RunningVM.TenantID label; no driver otherwise interprets it.
	TenantID          string
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
	AttachmentID string
	// TenantID is the owning VM's tenant_id (VolumeAttachment lives in the
	// same tenant) -- carried here only so agent.go's post-Boot report to
	// block-storage (see AttachedVolume) can address the right
	// VolumeAttachment; no driver ever interprets it itself.
	TenantID          string
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

// AttachedVolume is one VolumeAttachInfo actually resolved and wired into a
// VM by Boot -- see VMM.Boot's doc comment.
type AttachedVolume struct {
	AttachmentID string
	TenantID     string
	// DevicePath is the real, host-visible path volumeref.Resolve found
	// (before any jail-local placement) -- a block device path for ISCSI/
	// NVME_OF, or a file path for NFS.
	DevicePath string
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
