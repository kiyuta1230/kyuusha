// Package chvmm runs real cloud-hypervisor microVMs for compute-agent
// (driver_hint=CLOUD_HYPERVISOR) -- the second real VMM integration,
// alongside internal/compute-agent/fcvmm (driver_hint=FIRECRACKER).
// Replaces the earlier QEMU-based driver (2026-09-12): a real
// qemu-system-x86_64 binary turned out to need ~30 shared libraries and
// legacy PC firmware blobs staged into any jail (discovered while designing
// external jailing for it), none of which cloud-hypervisor needs. It's a
// single statically-linked binary (musl, `ldd` reports "statically linked"),
// has no BIOS/VGA-BIOS boot chain to carry (pure direct kernel boot via a
// PVH entry point, same ELF vmlinux fcvmm already caches), and ships
// seccomp on by default plus optional Landlock -- see
// docs/specs/cloud-hypervisor-boot.md for the full comparison and why this
// driver, unlike fcvmm, needs no external jailer at all.
//
// Boots from the exact same kind of Image as fcvmm (KERNEL_ROOTFS: a
// kernel + a raw rootfs filesystem, no bootloader, no partition table).
// Like fcvmm, this applies only host-side cgroup v2 CPU/memory limits
// (internal/compute-agent/cgroup), best-effort. Real network interfaces
// (internal/compute-agent/netsetup, the same tap/bridge wiring fcvmm uses)
// and a cloud-init NoCloud seed disk (internal/compute-agent/vmm's
// BuildSeedDisk) are both supported, same as fcvmm.
package chvmm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/netsetup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/volumeref"
)

var _ vmm.VMM = (*Manager)(nil)

const (
	// defaultBootArgs mirrors fcvmm's/the old qemuvmm's: cloud-hypervisor's
	// virtio-blk root disk shows up as /dev/vda, same convention.
	defaultBootArgs = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/init"

	// bootGracePeriod: see fcvmm's identical constant -- same reasoning,
	// just for cloud-hypervisor's own set of instant-exit failure modes
	// (missing binary, bad /dev/kvm permissions, a kernel image its PVH
	// loader rejects).
	bootGracePeriod = 500 * time.Millisecond

	killGracePeriod = 3 * time.Second

	// adoptedPollInterval: see fcvmm's identical constant -- same
	// reasoning (no *exec.Cmd to Wait() on for an adopted process).
	adoptedPollInterval = 2 * time.Second
)

// BootSpec and NetIface are aliases for internal/compute-agent/vmm's
// shapes -- see fcvmm's identical aliases for why these must be the exact
// same type, not merely structurally similar copies.
type BootSpec = vmm.BootSpec
type NetIface = vmm.NetIface

// Manager tracks the cloud-hypervisor processes this compute-agent has
// booted. One Manager per compute-agent process.
type Manager struct {
	// BinPath is the cloud-hypervisor binary to exec. Defaults to
	// "cloud-hypervisor" (resolved via $PATH) if empty.
	BinPath string
	// CacheDir holds downloaded kernel/rootfs artifacts, keyed by a hash of
	// their URL -- shared read-only across all VMs booted from the same
	// Image. Kept separate from fcvmm's own cache dir (not because the
	// artifacts differ -- a KERNEL_ROOTFS Image is identical either way --
	// but so each driver's on-disk state stays independently inspectable.
	// Defaults to /var/lib/kyuusha/ch-cache.
	CacheDir string
	// RunDir holds one subdirectory per running VM (its writable rootfs
	// copy, console log, and cloud-init seed disk if any). Defaults to
	// /var/lib/kyuusha/ch-run.
	RunDir string
	// StorageConnections is this host's declared set of storage
	// connections (see internal/compute-agent/volumeref), the same value
	// cmd/compute-agent/main.go also sends to RegisterHypervisor. Empty
	// means this host has none -- any VM with Volumes then fails to boot,
	// same as a missing kernel/rootfs URL would.
	StorageConnections volumeref.Connections

	downloadMu sync.Mutex // serializes ensureCached; fine at playground scale

	mu      sync.Mutex
	running map[string]*runningVM
}

// runningVM tracks what Boot did for one VM, so Stop (and the exit-watch
// goroutine Boot starts) can tear all of it down: the process itself, and
// every tap device Wire created for it. No Volume-side bookkeeping needed
// here (unlike fcvmm's runningVM): cloud-hypervisor opens a Volume's
// already-visible device/file path directly via --disk, with no jail to
// place it into and so nothing of this driver's own left to unwind at
// teardown.
type runningVM struct {
	// pid is this VM's real cloud-hypervisor process id -- set from
	// cmd.Process.Pid for a VM this Manager just booted, or from a
	// vmm.BootRecord's PID for one Reconcile adopted from a previous
	// compute-agent process. Signaled directly (via os.FindProcess, which
	// needs no parent/child relationship on Unix), not through cmd, since
	// an adopted VM has no *exec.Cmd here at all.
	pid int
	// exeBasename is what /proc/<pid>/exe should resolve to for pid to
	// still be this VM's cloud-hypervisor process, not an unrelated
	// process that has since reused the same pid -- see vmm.ProcessAlive.
	exeBasename string
	// tenantID is this VM's owning tenant (BootSpec.TenantID at Boot time,
	// or vmm.BootRecord.TenantID for one Reconcile adopted) -- carried only
	// for Running's RunningVM.TenantID label.
	tenantID string
	taps     []string
	// attached is what Boot resolved and returned for this VM -- kept so an
	// idempotent re-Boot (see Boot's top-of-function check) can return the
	// exact same result without re-resolving anything.
	attached []vmm.AttachedVolume
	// done is closed once the process has exited and its own cleanup (tap
	// teardown, cgroup removal) has finished -- by Boot's exit-watch
	// goroutine for a VM this Manager booted itself, or by watchAdopted for
	// one Reconcile adopted. Stop/Destroy block on it, same reasoning as
	// fcvmm's identical field.
	done chan struct{}
}

func (m *Manager) binPath() string {
	if m.BinPath != "" {
		return m.BinPath
	}
	return "cloud-hypervisor"
}

func (m *Manager) cacheDir() string {
	if m.CacheDir != "" {
		return m.CacheDir
	}
	return "/var/lib/kyuusha/ch-cache"
}

func (m *Manager) runDir() string {
	if m.RunDir != "" {
		return m.RunDir
	}
	return "/var/lib/kyuusha/ch-run"
}

// ConsoleLogPath is where Boot(vmID's spec) captures cloud-hypervisor's
// stdout/stderr (== the guest's serial console, ttyS0, via --serial tty
// below) -- see docs/specs/cloud-hypervisor-boot.md. It exists only once
// Boot has actually run for this vmID.
func (m *Manager) ConsoleLogPath(vmID string) string {
	return filepath.Join(m.runDir(), vmID, "console.log")
}

// Running implements vmm.VMM.
func (m *Manager) Running() []vmm.RunningVM {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]vmm.RunningVM, 0, len(m.running))
	for vmID, rv := range m.running {
		out = append(out, vmm.RunningVM{VMID: vmID, TenantID: rv.tenantID})
	}
	return out
}

// Reconcile adopts every VM under RunDir that has a boot record (see
// vmm.BootRecord) whose pid is still alive -- see fcvmm's identical
// Reconcile for the full reasoning (must run once, before this Manager
// accepts any commands).
func (m *Manager) Reconcile() {
	entries, err := os.ReadDir(m.runDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		vmID := e.Name()
		vmDir := filepath.Join(m.runDir(), vmID)
		rec, err := vmm.ReadBootRecord(vmDir)
		if err != nil {
			continue
		}
		if !vmm.ProcessAlive(rec.PID, rec.ExeBasename) {
			vmm.RemoveBootRecord(vmDir)
			continue
		}
		rv := &runningVM{
			pid: rec.PID, exeBasename: rec.ExeBasename, tenantID: rec.TenantID,
			taps: rec.Taps, attached: rec.Attached,
			done: make(chan struct{}),
		}
		m.mu.Lock()
		if m.running == nil {
			m.running = make(map[string]*runningVM)
		}
		m.running[vmID] = rv
		m.mu.Unlock()
		slog.Info("chvmm: adopted a VM still running from a previous compute-agent process", "vm_id", vmID, "pid", rec.PID)
		go m.watchAdopted(vmID, vmDir, rv)
	}
}

// watchAdopted polls an adopted VM's process until it exits, then runs the
// exact same teardown Boot's own exit-watch goroutine would have.
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
	for _, t := range rv.taps {
		_ = netsetup.DeleteTap(t)
	}
	if rmErr := cgroup.Remove(vmID); rmErr != nil {
		slog.Warn("chvmm: removing cgroup for adopted VM", "vm_id", vmID, "err", rmErr)
	}
	vmm.RemoveBootRecord(vmDir)
	slog.Info("chvmm: adopted VM's process exited", "vm_id", vmID)
	close(rv.done)
}

// Boot fetches (or reuses cached copies of) spec's kernel/rootfs, gives the
// VM its own writable rootfs copy and run directory, and starts
// cloud-hypervisor against them. It returns once cloud-hypervisor has
// either exited immediately (an error) or stayed up past bootGracePeriod
// (success) -- see that constant's doc for exactly what "success" does and
// doesn't mean.
func (m *Manager) Boot(ctx context.Context, spec BootSpec) ([]vmm.AttachedVolume, error) {
	// Idempotent no-op if spec.VMID is already tracked as running -- see
	// fcvmm's identical check for the full reasoning.
	m.mu.Lock()
	if existing, ok := m.running[spec.VMID]; ok {
		m.mu.Unlock()
		slog.Info("chvmm: boot requested for a vm_id already tracked as running, returning its existing result", "vm_id", spec.VMID, "pid", existing.pid)
		return existing.attached, nil
	}
	m.mu.Unlock()

	if err := os.MkdirAll(m.cacheDir(), 0o755); err != nil {
		return nil, fmt.Errorf("chvmm: create cache dir: %w", err)
	}
	kernelPath, err := m.ensureCached(ctx, spec.KernelURL)
	if err != nil {
		return nil, fmt.Errorf("chvmm: fetch kernel: %w", err)
	}
	masterRootfs, err := m.ensureCached(ctx, spec.RootfsURL)
	if err != nil {
		return nil, fmt.Errorf("chvmm: fetch rootfs: %w", err)
	}

	vmDir := filepath.Join(m.runDir(), spec.VMID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return nil, fmt.Errorf("chvmm: create run dir: %w", err)
	}

	// cloud-hypervisor's virtio-blk backend opens the drive read-write and
	// writes guest changes straight into the backing file, so every VM
	// needs its own copy -- the cached master is shared read-only across
	// VMs booted from the same Image. If this vm_id already has one (a
	// Start after Stop, see docs/architecture.md's VM lifecycle: Stop never
	// removes vmDir -- only Destroy does, called from Delete, not from
	// here), reuse it as-is instead of recopying from the Image, so the
	// guest's own writes since its last boot survive the restart.
	rootfsCopy := filepath.Join(vmDir, "rootfs.raw")
	if _, err := os.Stat(rootfsCopy); err != nil {
		if err := copyFile(masterRootfs, rootfsCopy); err != nil {
			return nil, fmt.Errorf("chvmm: copy rootfs: %w", err)
		}
	}

	// Wire every real network interface before cloud-hypervisor starts (it
	// opens each tap by name at boot, so it must already exist) -- same
	// internal/compute-agent/netsetup this VM's counterpart under fcvmm
	// uses; a tap device works identically regardless of which VMM process
	// ends up attached to it. On a failure partway through, unwire whatever
	// this call already created rather than leaking tap devices for a VM
	// that never boots.
	var netArgs []string
	var chNetArgs []string
	var taps []string
	var attached []vmm.AttachedVolume
	cleanup := func() {
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
	}
	for i, ni := range spec.NetworkInterfaces {
		wired, err := netsetup.Wire(netsetup.Interface{
			IfaceID:    ni.IfaceID,
			MACAddress: ni.MACAddress,
			GatewayIP:  ni.GatewayIP,
			PrefixLen:  ni.PrefixLen,
			VLANID:     ni.VLANID,
		})
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("chvmm: wire network interface %d (%s): %w", i, ni.IfaceID, err)
		}
		taps = append(taps, wired.TapName)
		chNetArgs = append(chNetArgs, "--net", fmt.Sprintf("tap=%s,mac=%s", wired.TapName, wired.MACAddress))
		// kyuusha.net.<i>.* is not a real kernel parameter: it's parsed by
		// the guest's own /init (docker/fc-guest-init.sh), exactly like
		// fcvmm's identical convention -- see docs/specs/network.md.
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

	diskArgs := []string{"--disk", fmt.Sprintf("path=%s", rootfsCopy)}
	if spec.UserData != "" {
		seedImg, err := vmm.BuildSeedDisk(vmDir, spec.VMID, spec.UserData, spec.NetworkInterfaces)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("chvmm: build seed disk: %w", err)
		}
		// Read-only, non-root: the guest sees this as a second virtio-blk
		// device (typically /dev/vdb) alongside its root disk, exactly what
		// cloud-init's NoCloud datasource expects -- same as fcvmm's seed
		// drive.
		diskArgs = append(diskArgs, "--disk", fmt.Sprintf("path=%s,readonly=on", seedImg))
	}

	// Discover every already-visible Volume before cloud-hypervisor starts
	// -- same reasoning as taps, see internal/compute-agent/volumeref and
	// docs/specs/volume.md. Each becomes its own read-write virtio-blk
	// drive alongside rootfs/seed, referenced by its real path directly:
	// no jail here for the path to need placing into.
	for i, v := range spec.Volumes {
		devPath, sizeBytes, err := volumeref.Resolve(m.StorageConnections, v.Protocol, v.StorageConnection, v.Identifier)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("chvmm: resolve volume %d (%s): %w", i, v.AttachmentID, err)
		}
		vmm.WarnIfSizeMismatch(v, sizeBytes)
		attached = append(attached, vmm.AttachedVolume{AttachmentID: v.AttachmentID, TenantID: v.TenantID, DevicePath: devPath})
		diskArgs = append(diskArgs, "--disk", fmt.Sprintf("path=%s", devPath))
	}

	consoleLog, err := os.Create(filepath.Join(vmDir, "console.log"))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("chvmm: create console log: %w", err)
	}
	defer consoleLog.Close()

	args := []string{
		"--kernel", kernelPath,
		"--cmdline", bootArgs,
		"--cpus", fmt.Sprintf("boot=%d", spec.VCPU),
		"--memory", fmt.Sprintf("size=%dM", spec.MemoryMB),
		// tty: writes straight to this process's own stdout/stderr (below),
		// same "console.log doubles as both the guest serial console and
		// wherever cloud-hypervisor's own startup errors land" contract as
		// fcvmm/the old qemuvmm's console.log. console=off: no second,
		// non-serial virtio-console device -- ttyS0 is the only I/O surface.
		"--serial", "tty",
		"--console", "off",
	}
	args = append(args, diskArgs...)
	args = append(args, chNetArgs...)

	// Not exec.CommandContext(ctx, ...): ctx here is the NATS message
	// handler's context, which is done long before this VM's guest is --
	// the process's lifetime is managed explicitly via m.running/Stop (same
	// reasoning as fcvmm).
	cmd := exec.Command(m.binPath(), args...)
	cmd.Stdout = consoleLog
	cmd.Stderr = consoleLog
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("chvmm: start cloud-hypervisor: %w", err)
	}

	// Best-effort: a host/container without usable cgroup v2 delegation just
	// boots this VM unconstrained -- see internal/compute-agent/cgroup's doc
	// comment.
	if err := cgroup.Apply(spec.VMID, spec.VCPU, spec.MemoryMB, cmd.Process.Pid); err != nil {
		slog.Warn("chvmm: cgroup limits not applied, VM will boot unconstrained", "vm_id", spec.VMID, "err", err)
	}

	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	select {
	case err := <-exitCh:
		cleanup()
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("chvmm: removing cgroup after immediate exit", "vm_id", spec.VMID, "err", rmErr)
		}
		return nil, fmt.Errorf("chvmm: cloud-hypervisor exited immediately (see %s): %w", consoleLog.Name(), err)
	case <-time.After(bootGracePeriod):
	}

	exeBasename := filepath.Base(m.binPath())
	rv := &runningVM{pid: cmd.Process.Pid, exeBasename: exeBasename, tenantID: spec.TenantID, taps: taps, attached: attached, done: make(chan struct{})}
	m.mu.Lock()
	if m.running == nil {
		m.running = make(map[string]*runningVM)
	}
	m.running[spec.VMID] = rv
	m.mu.Unlock()

	// Best-effort: losing this only costs this VM's restart-safety (see
	// Reconcile), not the correctness of the process actually running now.
	if err := vmm.WriteBootRecord(vmDir, vmm.BootRecord{
		PID: cmd.Process.Pid, ExeBasename: exeBasename, TenantID: spec.TenantID, Taps: taps, Attached: attached,
	}); err != nil {
		slog.Warn("chvmm: write boot record, this VM won't be adopted if compute-agent restarts", "vm_id", spec.VMID, "err", err)
	}

	go func() {
		err := <-exitCh
		m.mu.Lock()
		delete(m.running, spec.VMID)
		m.mu.Unlock()
		cleanup()
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("chvmm: removing cgroup", "vm_id", spec.VMID, "err", rmErr)
		}
		vmm.RemoveBootRecord(vmDir)
		if err != nil {
			slog.Warn("chvmm: cloud-hypervisor process exited", "vm_id", spec.VMID, "err", err)
		} else {
			slog.Info("chvmm: cloud-hypervisor process exited", "vm_id", spec.VMID)
		}
		close(rv.done)
	}()

	return attached, nil
}

// Stop tears down vmID's cloud-hypervisor process if one is running,
// blocking until it has actually exited -- see vmm.VMM's doc comment for
// why this must be synchronous, and fcvmm's identical Stop for the same
// reasoning. A no-op if this compute-agent never booted a real process for
// it (a VM some other driver booted, or one that already exited). Does not
// touch the VM's run directory (its root disk) -- see Destroy.
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
// entire run directory, including the root disk copy Boot placed there --
// the real teardown Delete needs (see docs/architecture.md's VM lifecycle
// and fcvmm's identical Destroy). A no-op if this compute-agent never
// booted a real process for vmID.
func (m *Manager) Destroy(vmID string) {
	m.Stop(vmID, true)
	if err := os.RemoveAll(filepath.Join(m.runDir(), vmID)); err != nil {
		slog.Warn("chvmm: destroy: remove run dir failed", "vm_id", vmID, "err", err)
	}
}

// ensureCached downloads rawURL into CacheDir if not already present, keyed
// by a hash of the URL itself -- identical in spirit to fcvmm's own
// ensureCached (deliberately not shared: each driver owns its own cache
// directory and its own small download helper, so neither depends on the
// other's internals).
func (m *Manager) ensureCached(ctx context.Context, rawURL string) (string, error) {
	if rawURL == "" {
		return "", fmt.Errorf("empty artifact URL")
	}
	sum := sha256.Sum256([]byte(rawURL))
	dest := filepath.Join(m.cacheDir(), hex.EncodeToString(sum[:16]))

	m.downloadMu.Lock()
	defer m.downloadMu.Unlock()

	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: unexpected status %s", rawURL, resp.Status)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("write %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
