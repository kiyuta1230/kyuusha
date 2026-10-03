// Package fcvmm runs real Firecracker microVMs for compute-agent, always
// via jailer -- see docs/specs/firecracker-boot.md "jailer" for the scope
// this covers and what it doesn't (network/PID namespace isolation and
// per-VM unique uid/gid are deliberately deferred; every VM's jail shares
// one fixed uid/gid). It does apply host-side cgroup v2 CPU/memory limits
// (see internal/compute-agent/cgroup) derived directly from spec.vcpu/
// spec.memory_mb, best-effort: a host/container without usable cgroup v2
// delegation just boots the VM unconstrained, same as before this existed.
// Manager implements internal/compute-agent/vmm.VMM -- see chvmm for the
// other implementation (driver_hint=CLOUD_HYPERVISOR). Real network interfaces
// (tap devices, per-VLAN bridges -- see internal/compute-agent/netsetup)
// are wired for VMs whose spec carries them; a VM with none boots exactly
// as before (network-less, serial-only). A VM with spec.user_data set gets
// a cloud-init NoCloud seed disk (see internal/compute-agent/vmm's
// BuildSeedDisk and docs/architecture.md "UserData注入: NoCloud seed
// disk"). Handles driver_hint=FIRECRACKER only; see
// internal/compute-agent/chvmm for driver_hint=CLOUD_HYPERVISOR, which boots
// from the exact same kind of Image (KERNEL_ROOTFS: a kernel + a raw
// rootfs, no bootloader) via a different VMM process.
package fcvmm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/imagestore"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/netsetup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/snap"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/volumeref"
)

// var _ vmm.VMM = (*Manager)(nil) is checked in manager_test.go-equivalent
// fashion by every real caller (agent.go stores Manager values in a
// map[string]vmm.VMM); asserted here too so a signature drift fails to
// compile immediately, at the point of the drift, not wherever it happens
// to be assigned.
var _ vmm.VMM = (*Manager)(nil)

const (
	// init=/init: the playground's guest rootfs (see docker/fc-guest-init.sh)
	// is a bare Alpine minirootfs with no working /sbin/init (no openrc
	// setup), so the kernel's default init search must be overridden.
	defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/init"

	// bootGracePeriod is how long Boot waits before declaring the
	// Firecracker process launched successfully. Long enough to catch the
	// failures that matter here (missing binary, bad /dev/kvm permissions,
	// a malformed config, a cached artifact that isn't actually a kernel/
	// ext4 image) -- these all make Firecracker exit within milliseconds.
	// It does NOT mean the guest kernel finished booting; that's out of
	// scope for this milestone (see docs/specs/firecracker-boot.md).
	bootGracePeriod = 500 * time.Millisecond

	killGracePeriod = 3 * time.Second

	// adoptedPollInterval is how often Reconcile's watchAdopted goroutine
	// checks whether an adopted VM's process has exited -- there's no
	// *exec.Cmd to Wait() on for a process this Manager didn't itself
	// fork, so liveness has to be polled instead of blocked on.
	adoptedPollInterval = 2 * time.Second
)

// BootSpec and NetIface are aliases (not new types) for
// internal/compute-agent/vmm's shapes: Manager implements vmm.VMM, and
// agent.go builds one shared vmm.BootSpec value regardless of which
// driver_hint it's dispatching to, so both must be the exact same type as
// what chvmm.Manager.Boot accepts, not merely structurally similar
// copies.
type BootSpec = vmm.BootSpec
type NetIface = vmm.NetIface

// Manager tracks the Firecracker processes this compute-agent has booted.
// One Manager per compute-agent process.
type Manager struct {
	// BinPath is the firecracker binary to exec. Defaults to "firecracker"
	// (resolved via $PATH) if empty.
	BinPath string
	// ImageStore caches downloaded kernel/rootfs artifacts, digest-verified
	// and shared read-only across every VM this compute-agent boots --
	// including chvmm's, since cmd/compute-agent/main.go constructs one
	// Store and hands it to both drivers. Must be set; a nil ImageStore
	// panics the first time Boot needs to fetch anything.
	ImageStore *imagestore.Store
	// RunDir holds one subdirectory per running VM (its console log --
	// everything else Firecracker itself touches now lives inside the
	// jail, see JailChrootBaseDir). Defaults to /var/lib/kyuusha/fc-run.
	RunDir string
	// JailerBinPath is the jailer binary to exec instead of Firecracker
	// directly. Defaults to "jailer" (resolved via $PATH) if empty.
	JailerBinPath string
	// JailChrootBaseDir is jailer's --chroot-base-dir: it creates
	// <JailChrootBaseDir>/<firecracker binary's basename>/<vm_id>/root for
	// each VM. Defaults to /var/lib/kyuusha/fc-jail.
	JailChrootBaseDir string
	// JailUID/JailGID are the uid/gid jailer drops privileges to before
	// exec'ing Firecracker inside the jail -- shared across every VM this
	// Manager boots (see the package doc comment for why per-VM unique
	// uid/gid is deferred). Zero (Go's zero value, and also root -- refusing
	// it is deliberate, not just a sentinel) means "use the default".
	// Default: 123/100, arbitrary but matching jailer's own docs.md example.
	JailUID uint32
	JailGID uint32

	// StorageConnections is this host's declared set of storage
	// connections (see internal/compute-agent/volumeref), the same value
	// cmd/compute-agent/main.go also sends to RegisterHypervisor. Empty
	// means this host has none -- any VM with Volumes then fails to boot,
	// same as a missing kernel/rootfs URL would.
	StorageConnections volumeref.Connections
	// NumaTopology maps this host's self-reported NUMA node ids to their
	// host logical CPU lists (the same facts sent to RegisterHypervisor's
	// numa_nodes) -- Boot uses it to resolve a scheduled spec.NumaNode into
	// the cgroup.NumaPin cgroup.Apply needs, since compute-agent already
	// has this data locally and there's no reason to round-trip it back
	// out through etcd/NATS (see numaPin).
	NumaTopology map[int32][]int32
	// NetworkAttachBin, if set, is the external VNAP plugin binary
	// netsetup.Wire/DeleteTap delegate the local tap-to-switch attach/
	// detach step to, instead of the built-in Linux bridge implementation
	// -- see internal/compute-agent/netsetup's package doc comment and
	// docs/architecture.md「VMのネットワーク接続をCNIのようにプラガブルに
	// すべきか」. Empty (the default) keeps today's behavior unchanged.
	NetworkAttachBin string
	// SecurityBackendBin, if set, is the external security-backend plugin
	// binary snap.Attach/Detach delegate ACL enforcement to, instead of
	// the built-in nftacl implementation -- see internal/compute-agent/
	// snap's package doc comment. Empty (the default) keeps ACL
	// enforcement in this process via nftacl.
	SecurityBackendBin string

	mu      sync.Mutex
	running map[string]*runningVM
}

// runningVM tracks what Boot did for one VM, so Stop (and the exit-watch
// goroutine Boot starts) can tear all of it down: the process itself,
// every tap device Wire created for it, and every Volume bind mount
// placeVolumeLike made (a Volume placed via mknodDeviceLike needs no such
// cleanup -- removing a block device special file never touches the real
// device behind it).
type runningVM struct {
	// pid is this VM's real Firecracker process id -- set from
	// cmd.Process.Pid for a VM this Manager just booted, or from a
	// vmm.BootRecord's PID for one Reconcile adopted from a previous
	// compute-agent process's boot. Signaled directly (via os.FindProcess,
	// which needs no parent/child relationship on Unix), not through cmd,
	// since an adopted VM has no *exec.Cmd here at all.
	pid int
	// exeBasename is what /proc/<pid>/exe should resolve to for pid to
	// still be this VM's Firecracker process, not an unrelated process that
	// has since reused the same pid -- see vmm.ProcessAlive.
	exeBasename string
	// tenantID is this VM's owning tenant (BootSpec.TenantID at Boot time,
	// or vmm.BootRecord.TenantID for one Reconcile adopted) -- carried only
	// for Running's RunningVM.TenantID label.
	tenantID     string
	taps         []string
	volumeMounts []string
	// ifaceIDs are the NetworkInterface ids taps was wired from, in the
	// same order -- see vmm.RunningVM.NetworkInterfaces.
	ifaceIDs []string
	// attached is what Boot resolved and returned for this VM -- kept so an
	// idempotent re-Boot (see Boot's top-of-function check) can return the
	// exact same result without re-resolving anything.
	attached []vmm.AttachedVolume
	// pinnedKeys are the imagestore.Store cache keys Pinned for this VM --
	// see vmm.BootRecord.PinnedKeys. Unpinned once this VM is removed from
	// m.running (exit-watch or watchAdopted).
	pinnedKeys []string
	// done is closed once the process has exited AND its own cleanup (tap/
	// volume-mount teardown, cgroup removal) has finished -- by Boot's
	// exit-watch goroutine for a VM this Manager booted itself, or by
	// watchAdopted for one Reconcile adopted. Stop/Destroy block on it so
	// neither returns, nor (for Destroy) removes the jail directory, while a
	// bind-mounted Volume might still be mounted underneath it.
	done chan struct{}
}

func (m *Manager) binPath() string {
	if m.BinPath != "" {
		return m.BinPath
	}
	return "firecracker"
}

func (m *Manager) runDir() string {
	if m.RunDir != "" {
		return m.RunDir
	}
	return "/var/lib/kyuusha/fc-run"
}

func (m *Manager) jailerBinPath() string {
	if m.JailerBinPath != "" {
		return m.JailerBinPath
	}
	return "jailer"
}

func (m *Manager) jailChrootBaseDir() string {
	if m.JailChrootBaseDir != "" {
		return m.JailChrootBaseDir
	}
	return "/var/lib/kyuusha/fc-jail"
}

func (m *Manager) jailUID() uint32 {
	if m.JailUID != 0 {
		return m.JailUID
	}
	return 123
}

func (m *Manager) jailGID() uint32 {
	if m.JailGID != 0 {
		return m.JailGID
	}
	return 100
}

// numaPin resolves a scheduled BootSpec.NumaNode into the cgroup.NumaPin
// cgroup.Apply needs, via this Manager's own detected NumaTopology -- nil
// (no pinning) for vmm.UnpinnedNumaNode or a node id NumaTopology doesn't
// recognize (topology detection failed or changed since scheduling; Apply
// then just boots unconstrained on that axis, same best-effort spirit as
// every other cgroup.Apply failure).
func (m *Manager) numaPin(nodeID int32) *cgroup.NumaPin {
	if nodeID < 0 {
		return nil
	}
	cpus, ok := m.NumaTopology[nodeID]
	if !ok {
		return nil
	}
	return &cgroup.NumaPin{NodeID: nodeID, CPUs: cpus}
}

// RootDiskPath implements vmm.VMM -- see that interface's doc comment.
// Mirrors Boot's own chroot/rootfsCopy path construction exactly (jailer.go's
// jailChrootDir), a pure function of vmID and this Manager's own config, not
// runtime state.
func (m *Manager) RootDiskPath(vmID string) (string, error) {
	fcExecPath, err := resolveExecPath(m.binPath())
	if err != nil {
		return "", fmt.Errorf("fcvmm: resolve firecracker binary: %w", err)
	}
	path := filepath.Join(jailChrootDir(m.jailChrootBaseDir(), fcExecPath, vmID), "rootfs.ext4")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("fcvmm: root disk for vm_id %q: %w", vmID, err)
	}
	return path, nil
}

// ApplyACL looks up ifaceID's tap among vmID's already-wired interfaces (if
// any) and, only if found, re-applies its ACL state via snap.Attach --
// see vmm.VMM's own doc comment for the applied=false/no-tap-yet contract.
func (m *Manager) ApplyACL(vmID string, u vmm.ACLUpdate) (bool, error) {
	m.mu.Lock()
	rv, ok := m.running[vmID]
	var tap, tenantID string
	if ok {
		tenantID = rv.tenantID
		for i, id := range rv.ifaceIDs {
			if id == u.IfaceID {
				tap = rv.taps[i]
				break
			}
		}
	}
	m.mu.Unlock()
	if tap == "" {
		return false, nil
	}
	err := snap.Attach(snap.Interface{
		IfaceID: u.IfaceID, VMID: vmID, TenantID: tenantID, TapName: tap,
		SubnetID: u.SubnetID, SubnetCIDR: u.SubnetCIDR, GatewayIP: u.GatewayIP,
		IPAddress: u.IPAddress, MACAddress: u.MACAddress,
		IngressRules: toSnapRules(u.IngressRules), EgressRules: toSnapRules(u.EgressRules),
	}, m.SecurityBackendBin)
	return true, err
}

func toSnapRules(rules []vmm.FirewallRule) []snap.FirewallRule {
	var out []snap.FirewallRule
	for _, r := range rules {
		out = append(out, snap.FirewallRule{Protocol: r.Protocol, PortRange: r.PortRange, SourceCIDR: r.SourceCIDR, Action: r.Action})
	}
	return out
}

// ConsoleLogPath is where Boot(vmID's spec) captures Firecracker's stdout/
// stderr (== the guest's serial console, ttyS0) -- see docs/specs/
// firecracker-boot.md. It exists only once Boot has actually run for this
// vmID (never, for a stub-succeeded CLOUD_HYPERVISOR VM or one that hasn't
// booted yet).
func (m *Manager) ConsoleLogPath(vmID string) string {
	return filepath.Join(m.runDir(), vmID, "console.log")
}

// Running implements vmm.VMM.
func (m *Manager) Running() []vmm.RunningVM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vmm.RunningVM, 0, len(m.running))
	for vmID, rv := range m.running {
		out = append(out, vmm.RunningVM{
			VMID: vmID, TenantID: rv.tenantID, PID: rv.pid,
			NetworkInterfaces: rv.ifaceIDs, Volumes: rv.attached,
		})
	}
	return out
}

// Reconcile adopts every VM under RunDir that has a boot record (see
// vmm.BootRecord) whose pid is still alive, populating m.running for it as
// if this Manager had just booted it itself. Must be called once, before
// this Manager accepts any commands (see cmd/compute-agent/main.go and
// computeagent.Agent.Run) -- otherwise a compute-agent restart would
// silently forget every VM it had previously booted that's still running
// (Stop/Destroy would no-op for them forever), and a resent Boot for one
// of them would wipe its live jail chroot out from under it. A boot
// record whose pid is gone (the VM actually exited, e.g. while
// compute-agent itself was down) is just removed -- nothing to adopt.
func (m *Manager) Reconcile() {
	entries, err := os.ReadDir(m.runDir())
	if err != nil {
		return // no run dir yet: nothing has ever booted here
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		vmID := e.Name()
		vmDir := filepath.Join(m.runDir(), vmID)
		rec, err := vmm.ReadBootRecord(vmDir)
		if err != nil {
			continue // no record (never booted for real, or already cleaned up)
		}
		if !vmm.ProcessAlive(rec.PID, rec.ExeBasename) {
			vmm.RemoveBootRecord(vmDir)
			continue
		}
		rv := &runningVM{
			pid: rec.PID, exeBasename: rec.ExeBasename, tenantID: rec.TenantID,
			taps: rec.Taps, volumeMounts: rec.VolumeMounts, ifaceIDs: rec.NetworkInterfaces, attached: rec.Attached,
			pinnedKeys: rec.PinnedKeys,
			done:       make(chan struct{}),
		}
		m.mu.Lock()
		if m.running == nil {
			m.running = make(map[string]*runningVM)
		}
		m.running[vmID] = rv
		m.mu.Unlock()
		// Re-establish the pin an earlier compute-agent process's Boot took
		// out (see vmm.BootRecord.PinnedKeys' doc comment) -- this process's
		// own imagestore.Store starts with none, so without this an adopted
		// VM's still-in-use kernel blob would look unreferenced to Sweep.
		m.ImageStore.Pin(rec.PinnedKeys...)
		slog.Info("fcvmm: adopted a VM still running from a previous compute-agent process", "vm_id", vmID, "pid", rec.PID)
		go m.watchAdopted(vmID, vmDir, rv)
	}
}

// watchAdopted polls an adopted VM's process until it exits, then runs the
// exact same teardown Boot's own exit-watch goroutine would have -- see
// runningVM.done's doc comment.
func (m *Manager) watchAdopted(vmID, vmDir string, rv *runningVM) {
	ticker := time.NewTicker(adoptedPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !vmm.ProcessAlive(rv.pid, rv.exeBasename) {
			break
		}
	}
	m.mu.Lock()
	delete(m.running, vmID)
	m.mu.Unlock()
	m.ImageStore.Unpin(rv.pinnedKeys...)
	for i, t := range rv.taps {
		ifaceID := ""
		if i < len(rv.ifaceIDs) {
			ifaceID = rv.ifaceIDs[i]
		}
		_ = snap.Detach(ifaceID, vmID, rv.tenantID, t, m.SecurityBackendBin)
		_ = netsetup.DeleteTap(t, ifaceID, vmID, rv.tenantID, m.NetworkAttachBin)
	}
	for _, p := range rv.volumeMounts {
		_ = unix.Unmount(p, 0)
	}
	if rmErr := cgroup.Remove(vmID); rmErr != nil {
		slog.Warn("fcvmm: removing cgroup for adopted VM", "vm_id", vmID, "err", rmErr)
	}
	vmm.RemoveBootRecord(vmDir)
	slog.Info("fcvmm: adopted VM's process exited", "vm_id", vmID)
	close(rv.done)
}

// Boot fetches (or reuses cached copies of) spec's kernel/rootfs, gives the
// VM its own writable rootfs copy and run directory, and starts Firecracker
// against them. It returns once Firecracker has either exited immediately
// (an error) or stayed up past bootGracePeriod (success) -- see that
// constant's doc for exactly what "success" does and doesn't mean.
func (m *Manager) Boot(ctx context.Context, spec BootSpec) ([]vmm.AttachedVolume, error) {
	// Idempotent no-op if spec.VMID is already tracked as running -- either
	// booted earlier in this same process's life, or adopted by Reconcile
	// at startup from a previous compute-agent process. Without this, a
	// resent CreateCommand (e.g. a future retry for a VM stuck in
	// Provisioning) would blindly wipe this VM's live jail chroot and start
	// a second, genuinely-duplicate Firecracker process for the same
	// vm_id. m.running only ever holds entries believed alive (removed the
	// moment exit-watch/watchAdopted observes the process gone), so no
	// extra liveness check is needed here -- worst case is a bounded
	// staleness window of one adoptedPollInterval for an adopted VM whose
	// process just died.
	m.mu.Lock()
	if existing, ok := m.running[spec.VMID]; ok {
		m.mu.Unlock()
		slog.Info("fcvmm: boot requested for a vm_id already tracked as running, returning its existing result", "vm_id", spec.VMID, "pid", existing.pid)
		return existing.attached, nil
	}
	m.mu.Unlock()

	kernelPath, err := m.ImageStore.EnsureCached(ctx, spec.KernelURL, spec.KernelDigest)
	if err != nil {
		return nil, fmt.Errorf("fcvmm: fetch kernel: %w", err)
	}
	masterRootfs, err := m.ImageStore.EnsureCached(ctx, spec.RootfsURL, spec.RootfsDigest)
	if err != nil {
		return nil, fmt.Errorf("fcvmm: fetch rootfs: %w", err)
	}

	// Pinned once this VM is confirmed running (below) so imagestore.Sweep
	// never evicts the kernel out from under a live Firecracker process --
	// see imagestore.Store.Pin's doc comment for why only the kernel (not
	// masterRootfs, which is never read again after CloneFile-ing it into
	// this VM's own writable copy) needs one.
	kernelKey, _ := m.ImageStore.Key(spec.KernelURL, spec.KernelDigest)
	var pinnedKeys []string
	if kernelKey != "" {
		pinnedKeys = []string{kernelKey}
	}

	// console.log is the only thing this VM still keeps outside the jail --
	// it's just an *os.File handed to the child as fd 1/2 before jailer
	// chroots, which chroot/pivot_root doesn't affect (already-open file
	// descriptors survive it).
	vmDir := filepath.Join(m.runDir(), spec.VMID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return nil, fmt.Errorf("fcvmm: create run dir: %w", err)
	}

	fcExecPath, err := resolveExecPath(m.binPath())
	if err != nil {
		return nil, fmt.Errorf("fcvmm: resolve firecracker binary: %w", err)
	}
	jailUID, jailGID := m.jailUID(), m.jailGID()
	chroot := jailChrootDir(m.jailChrootBaseDir(), fcExecPath, spec.VMID)
	rootfsCopy := filepath.Join(chroot, "rootfs.ext4")

	// A restart (Start after Stop) finds this VM's chroot already populated
	// from its previous boot -- but jailer itself does NOT tolerate that:
	// it unconditionally mknods dev/net/tun (and similar) on every
	// invocation and errors (EEXIST) if the chroot already has them from
	// before (confirmed: "Failed to create /dev/net/tun via mknod inside
	// the jail: File exists"). Only rootfs.ext4 -- the actual root disk --
	// needs to survive a restart (see docs/architecture.md's VM lifecycle);
	// everything else jailer owns (dev/, run/, its own copy of the
	// Firecracker binary, firecracker.pid) must be wiped so jailer sees a
	// chroot it recognizes as fresh. Preserve just the rootfs across that
	// wipe by moving it out and back. tmp must live OUTSIDE chroot (chroot's
	// parent directory, which os.RemoveAll(chroot) below never touches) --
	// placing it inside chroot itself (as a first version of this fix did)
	// gets it deleted right along with everything else being wiped.
	var preservedRootfs string
	if _, err := os.Stat(rootfsCopy); err == nil {
		tmp := filepath.Join(filepath.Dir(chroot), "rootfs.ext4.reuse")
		if err := os.Rename(rootfsCopy, tmp); err != nil {
			return nil, fmt.Errorf("fcvmm: preserve existing rootfs before rebuilding jail: %w", err)
		}
		preservedRootfs = tmp
	}
	if err := os.RemoveAll(chroot); err != nil {
		return nil, fmt.Errorf("fcvmm: clear jail dir: %w", err)
	}
	if err := os.MkdirAll(chroot, 0o755); err != nil {
		return nil, fmt.Errorf("fcvmm: create jail chroot dir: %w", err)
	}
	if preservedRootfs != "" {
		if err := os.Rename(preservedRootfs, rootfsCopy); err != nil {
			return nil, fmt.Errorf("fcvmm: restore reused rootfs: %w", err)
		}
	}

	// jailer copies the Firecracker binary itself in, but nothing else --
	// every resource Firecracker's config references has to already be
	// inside the chroot before jailer ever runs (see docs/specs/
	// firecracker-boot.md "jailer"), referenced by its chroot-relative
	// path, not this container's own view of it.
	kernelInJail := filepath.Join(chroot, "kernel")
	if err := placeReadOnlyResource(kernelPath, kernelInJail); err != nil {
		return nil, fmt.Errorf("fcvmm: place kernel in jail: %w", err)
	}

	// Firecracker opens its root drive read-write and writes guest changes
	// straight into the backing file, so every VM needs its own copy -- the
	// cached master is shared read-only across VMs booted from the same
	// Image. rootfsCopy having just survived the wipe above (a restart)
	// means the guest's own writes since its last boot are reused as-is
	// instead of recopying from the Image; only a genuinely new VM (or one
	// whose jail was somehow lost between boots) gets a fresh copy here.
	if _, err := os.Stat(rootfsCopy); err != nil {
		if err := placeWritableResource(masterRootfs, rootfsCopy, jailUID, jailGID); err != nil {
			return nil, fmt.Errorf("fcvmm: copy rootfs into jail: %w", err)
		}
	}

	apiSock := filepath.Join(chroot, "api.sock")
	_ = os.Remove(apiSock) // stale socket from a previous failed attempt, if any; Firecracker refuses to start if this exists

	// Wire every real network interface before Firecracker starts (it opens
	// each host_dev_name by name at boot, so the tap must already exist).
	// On a failure partway through, cleanup() unwinds whatever's already
	// been wired/placed rather than leaking tap devices or bind mounts for
	// a VM that never boots.
	var fcNetIfaces []fcNetworkInterface
	var netArgs []string
	var taps []string
	var ifaceIDs []string
	var volumeMounts []string
	var attached []vmm.AttachedVolume
	cleanup := func() {
		for i, t := range taps {
			ifaceID := ""
			if i < len(ifaceIDs) {
				ifaceID = ifaceIDs[i]
			}
			_ = snap.Detach(ifaceID, spec.VMID, spec.TenantID, t, m.SecurityBackendBin)
			_ = netsetup.DeleteTap(t, ifaceID, spec.VMID, spec.TenantID, m.NetworkAttachBin)
		}
		for _, p := range volumeMounts {
			_ = unix.Unmount(p, 0)
		}
	}
	for i, ni := range spec.NetworkInterfaces {
		wired, err := netsetup.Wire(netsetup.Interface{
			IfaceID:    ni.IfaceID,
			VMID:       spec.VMID,
			TenantID:   spec.TenantID,
			SubnetID:   ni.SubnetID,
			Zone:       ni.Zone,
			MACAddress: ni.MACAddress,
			IPAddress:  ni.IPAddress,
			SubnetCIDR: ni.SubnetCIDR,
			GatewayIP:  ni.GatewayIP,
			PrefixLen:  ni.PrefixLen,
			VLANID:     ni.VLANID,
			Primary:    ni.Primary,
		}, m.NetworkAttachBin)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("fcvmm: wire network interface %d (%s): %w", i, ni.IfaceID, err)
		}
		taps = append(taps, wired.TapName)
		ifaceIDs = append(ifaceIDs, ni.IfaceID)
		if err := snap.Attach(snap.Interface{
			IfaceID: ni.IfaceID, VMID: spec.VMID, TenantID: spec.TenantID, TapName: wired.TapName,
			SubnetID: ni.SubnetID, SubnetCIDR: ni.SubnetCIDR, GatewayIP: ni.GatewayIP,
			IPAddress: ni.IPAddress, MACAddress: wired.MACAddress,
			IngressRules: toSnapRules(ni.IngressRules), EgressRules: toSnapRules(ni.EgressRules),
		}, m.SecurityBackendBin); err != nil {
			cleanup()
			return nil, fmt.Errorf("fcvmm: apply ACL for network interface %d (%s): %w", i, ni.IfaceID, err)
		}
		fcNetIfaces = append(fcNetIfaces, fcNetworkInterface{
			IfaceID:     fmt.Sprintf("eth%d", i),
			GuestMAC:    wired.MACAddress,
			HostDevName: wired.TapName,
		})
		// kyuusha.net.<i>.* is not a real kernel parameter: it's parsed by
		// this guest's own /init (docker/fc-guest-init.sh), which is why
		// static IP configuration can travel this way instead of needing a
		// DHCP server or kernel IP autoconfiguration support.
		netArgs = append(netArgs, fmt.Sprintf("kyuusha.net.%d.ip=%s/%d", i, ni.IPAddress, ni.PrefixLen))
		if ni.GatewayIP != "" {
			netArgs = append(netArgs, fmt.Sprintf("kyuusha.net.%d.gw=%s", i, ni.GatewayIP))
		}
		if ni.Primary {
			netArgs = append(netArgs, fmt.Sprintf("kyuusha.net.%d.primary=1", i))
		}
	}

	bootArgs := spec.BootArgs
	if bootArgs == "" {
		bootArgs = defaultBootArgs
	}
	if len(netArgs) > 0 {
		bootArgs = bootArgs + " " + strings.Join(netArgs, " ")
	}

	// PathOnHost below is, despite the name (Firecracker's own API field),
	// the path *as seen from inside the jail* -- Firecracker reads
	// config.json only after jailer has already chrooted it, so every path
	// here is chroot-relative, not this container's own view of it.
	drives := []fcDrive{{
		DriveID:      "rootfs",
		PathOnHost:   "/rootfs.ext4",
		IsRootDevice: true,
		IsReadOnly:   false,
	}}
	if spec.UserData != "" {
		seedImg, err := vmm.BuildSeedDisk(chroot, spec.VMID, spec.UserData, spec.NetworkInterfaces)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("fcvmm: build seed disk: %w", err)
		}
		if err := os.Chmod(seedImg, 0o644); err != nil {
			cleanup()
			return nil, fmt.Errorf("fcvmm: chmod seed disk: %w", err)
		}
		// Read-only, non-root: the guest sees this as a second
		// virtio-block device (typically /dev/vdb) alongside its root
		// disk, exactly what cloud-init's NoCloud datasource expects.
		drives = append(drives, fcDrive{
			DriveID:      "seed",
			PathOnHost:   "/" + filepath.Base(seedImg),
			IsRootDevice: false,
			IsReadOnly:   true,
		})
	}

	// Discover every already-visible Volume before Firecracker starts (same
	// reasoning as taps: the device/file must exist before a drive can
	// reference it) -- see internal/compute-agent/volumeref and
	// docs/specs/volume.md. Each becomes its own read-write virtio-block
	// drive alongside rootfs/seed, placed via placeVolumeLike (mknod for a
	// block device, bind mount for an NFS file -- either way, guest writes
	// land on the real backing store, not a jail-local copy).
	for i, v := range spec.Volumes {
		devPath, sizeBytes, err := volumeref.Resolve(m.StorageConnections, v.Protocol, v.StorageConnection, v.Identifier)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("fcvmm: resolve volume %d (%s): %w", i, v.AttachmentID, err)
		}
		vmm.WarnIfSizeMismatch(v, sizeBytes)
		attached = append(attached, vmm.AttachedVolume{AttachmentID: v.AttachmentID, TenantID: v.TenantID, DevicePath: devPath})
		volName := fmt.Sprintf("vol%d", i)
		dst := filepath.Join(chroot, volName)
		mounted, err := placeVolumeLike(devPath, dst, jailUID, jailGID)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("fcvmm: place volume %d (%s) into jail: %w", i, v.AttachmentID, err)
		}
		if mounted {
			volumeMounts = append(volumeMounts, dst)
		}
		drives = append(drives, fcDrive{
			DriveID:      volName,
			PathOnHost:   "/" + volName,
			IsRootDevice: false,
			IsReadOnly:   false,
		})
	}

	cfg := fcConfig{
		BootSource:        fcBootSource{KernelImagePath: "/kernel", BootArgs: bootArgs},
		Drives:            drives,
		MachineConfig:     fcMachineConfig{VcpuCount: spec.VCPU, MemSizeMib: spec.MemoryMB},
		NetworkInterfaces: fcNetIfaces,
	}
	configPath := filepath.Join(chroot, "config.json")
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("fcvmm: marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o644); err != nil {
		cleanup()
		return nil, fmt.Errorf("fcvmm: write config: %w", err)
	}

	consoleLog, err := os.Create(filepath.Join(vmDir, "console.log"))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("fcvmm: create console log: %w", err)
	}
	defer consoleLog.Close()

	// Not exec.CommandContext(ctx, ...): ctx here is the NATS message
	// handler's context, which is done long before this VM's guest is --
	// the process's lifetime is managed explicitly via m.running/Stop.
	//
	// No --netns/--new-pid-ns: this VM's tap devices (see netsetup.Wire
	// above) live in this compute-agent container's own network namespace,
	// and every Volume this VM references is already visible in it too
	// (kyuusha never logs in/mounts anything itself -- see
	// internal/compute-agent/volumeref) -- adding a network namespace into
	// the mix here is deliberately out of scope for now (see the package
	// doc comment). jailer execs straight into Firecracker without
	// forking when neither flag is given, so cmd.Process.Pid below still
	// names the Firecracker process itself, same as it did calling it
	// directly.
	cmd := exec.Command(m.jailerBinPath(),
		"--id", spec.VMID,
		"--exec-file", fcExecPath,
		"--uid", fmt.Sprint(jailUID),
		"--gid", fmt.Sprint(jailGID),
		"--chroot-base-dir", m.jailChrootBaseDir(),
		"--",
		"--api-sock", "/"+filepath.Base(apiSock),
		"--config-file", "/"+filepath.Base(configPath),
	)
	cmd.Stdout = consoleLog
	cmd.Stderr = consoleLog
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("fcvmm: start jailer: %w", err)
	}

	// Best-effort: a host/container without usable cgroup v2 delegation just
	// boots this VM unconstrained, same as before this existed -- see
	// internal/compute-agent/cgroup's doc comment.
	if err := cgroup.Apply(spec.VMID, spec.VCPU, spec.MemoryMB, cmd.Process.Pid, m.numaPin(spec.NumaNode)); err != nil {
		slog.Warn("fcvmm: cgroup limits not applied, VM will boot unconstrained", "vm_id", spec.VMID, "err", err)
	}

	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	select {
	case err := <-exitCh:
		cleanup()
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("fcvmm: removing cgroup after immediate exit", "vm_id", spec.VMID, "err", rmErr)
		}
		return nil, fmt.Errorf("fcvmm: firecracker exited immediately (see %s): %w", consoleLog.Name(), err)
	case <-time.After(bootGracePeriod):
	}

	exeBasename := filepath.Base(fcExecPath)
	rv := &runningVM{pid: cmd.Process.Pid, exeBasename: exeBasename, tenantID: spec.TenantID, taps: taps, volumeMounts: volumeMounts, ifaceIDs: ifaceIDs, attached: attached, pinnedKeys: pinnedKeys, done: make(chan struct{})}
	m.mu.Lock()
	if m.running == nil {
		m.running = make(map[string]*runningVM)
	}
	m.running[spec.VMID] = rv
	m.mu.Unlock()
	m.ImageStore.Pin(pinnedKeys...)

	// Best-effort: losing this only costs this VM's restart-safety (see
	// Reconcile), not the correctness of the process actually running now.
	if err := vmm.WriteBootRecord(vmDir, vmm.BootRecord{
		PID: cmd.Process.Pid, ExeBasename: exeBasename, TenantID: spec.TenantID, Taps: taps, VolumeMounts: volumeMounts, NetworkInterfaces: ifaceIDs, Attached: attached, PinnedKeys: pinnedKeys,
	}); err != nil {
		slog.Warn("fcvmm: write boot record, this VM won't be adopted if compute-agent restarts", "vm_id", spec.VMID, "err", err)
	}

	go func() {
		err := <-exitCh
		m.mu.Lock()
		delete(m.running, spec.VMID)
		m.mu.Unlock()
		m.ImageStore.Unpin(pinnedKeys...)
		cleanup()
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("fcvmm: removing cgroup", "vm_id", spec.VMID, "err", rmErr)
		}
		vmm.RemoveBootRecord(vmDir)
		if err != nil {
			slog.Warn("fcvmm: firecracker process exited", "vm_id", spec.VMID, "err", err)
		} else {
			slog.Info("fcvmm: firecracker process exited", "vm_id", spec.VMID)
		}
		close(rv.done)
	}()

	return attached, nil
}

// Stop tears down vmID's Firecracker process if one is running, blocking
// until it has actually exited (and this Manager's own cleanup for it has
// finished -- see runningVM.done) -- see vmm.VMM's doc comment for why this
// must be synchronous. A no-op if this compute-agent never booted a real
// process for it (stub-succeeded path, or it already exited). Does not
// touch the jail/run directory -- see Destroy.
func (m *Manager) Stop(vmID string, force bool) {
	m.mu.Lock()
	rv, ok := m.running[vmID]
	m.mu.Unlock()
	if !ok {
		return
	}
	proc, _ := os.FindProcess(rv.pid) // Unix: always succeeds regardless of parent/child relationship
	if force {
		_ = proc.Signal(syscall.SIGKILL)
		<-rv.done
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-rv.done:
	case <-time.After(killGracePeriod):
		_ = proc.Signal(syscall.SIGKILL) // no-op if it already exited on SIGTERM
		<-rv.done
	}
}

// Destroy stops vmID (forcefully, if still running) and then removes its
// entire jail directory, including the root disk Boot placed there -- the
// real teardown Delete needs (see docs/architecture.md's VM lifecycle:
// unlike Stop, Delete must not leave the disk behind). A no-op (nothing to
// remove) if this compute-agent never booted a real process for vmID: it
// never got a jail directory at all.
func (m *Manager) Destroy(vmID string) {
	m.Stop(vmID, true)

	fcExecPath, err := resolveExecPath(m.binPath())
	if err != nil {
		slog.Warn("fcvmm: destroy: resolve firecracker binary, cannot locate jail to remove", "vm_id", vmID, "err", err)
		return
	}
	// jailChrootDir returns .../<vmID>/root; its parent is everything jailer
	// created for this VM, all of which is safe to remove now that the
	// process is confirmed gone.
	vmJailDir := filepath.Dir(jailChrootDir(m.jailChrootBaseDir(), fcExecPath, vmID))
	if err := os.RemoveAll(vmJailDir); err != nil {
		slog.Warn("fcvmm: destroy: remove jail dir failed", "vm_id", vmID, "dir", vmJailDir, "err", err)
	}
	if err := os.RemoveAll(filepath.Join(m.runDir(), vmID)); err != nil {
		slog.Warn("fcvmm: destroy: remove run dir failed", "vm_id", vmID, "err", err)
	}
}

type fcBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type fcDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type fcMachineConfig struct {
	VcpuCount  int32 `json:"vcpu_count"`
	MemSizeMib int64 `json:"mem_size_mib"`
}

type fcNetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMAC    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

type fcConfig struct {
	BootSource        fcBootSource         `json:"boot-source"`
	Drives            []fcDrive            `json:"drives"`
	MachineConfig     fcMachineConfig      `json:"machine-config"`
	NetworkInterfaces []fcNetworkInterface `json:"network-interfaces,omitempty"`
}
