// Package qemuvmm runs real QEMU microVMs for compute-agent
// (driver_hint=QEMU) -- the second real VMM integration, alongside
// internal/compute-agent/fcvmm (driver_hint=FIRECRACKER). See
// docs/specs/qemu-boot.md for what this covers and doesn't, and why QEMU
// exists here at all given fcvmm already works: PCI passthrough (VFIO) and
// vhost-user networking are only possible through QEMU's fuller device
// model, which Firecracker's deliberately minimal one doesn't have --
// neither is implemented yet, but the machine type chosen below (q35, a
// real PCIe bus) is chosen specifically to not foreclose them later.
//
// Boots from the exact same kind of Image as fcvmm (KERNEL_ROOTFS: a
// kernel + a raw rootfs filesystem, no bootloader, no partition table) via
// QEMU's own direct Linux boot protocol (-kernel/-append) instead of
// Firecracker's config-file boot -- see docs/specs/qemu-boot.md for why
// this, and not a self-contained bootable disk image (QCOW2), was chosen
// for this first pass, and what that rules out (non-Linux guests, notably
// Windows). Like fcvmm, this deliberately skips jailer-equivalent process
// isolation (chroot/namespace/uid-drop) and applies only host-side cgroup
// v2 CPU/memory limits (internal/compute-agent/cgroup), best-effort. Real
// network interfaces (internal/compute-agent/netsetup, the same tap/bridge
// wiring fcvmm uses) and a cloud-init NoCloud seed disk
// (internal/compute-agent/vmm's BuildSeedDisk) are both supported, same as
// fcvmm.
package qemuvmm

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
	// defaultBootArgs mirrors fcvmm's, with one real difference: QEMU has
	// no equivalent of Firecracker's is_root_device flag auto-injecting
	// `root=/dev/vda` into the kernel cmdline, so it must be spelled out
	// here explicitly. No `pci=off` either -- unlike Firecracker, this
	// driver's machine type (q35) has a real PCI bus, and disabling PCI
	// probing would also take virtio-blk/virtio-net's PCI transport down
	// with it.
	defaultBootArgs = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/init"

	// bootGracePeriod: see fcvmm's identical constant -- same reasoning,
	// just for QEMU's own set of instant-exit failure modes (missing
	// binary, bad /dev/kvm permissions, a kernel image QEMU's direct-boot
	// loader rejects).
	bootGracePeriod = 500 * time.Millisecond

	killGracePeriod = 3 * time.Second
)

// BootSpec and NetIface are aliases for internal/compute-agent/vmm's
// shapes -- see fcvmm's identical aliases for why these must be the exact
// same type, not merely structurally similar copies.
type BootSpec = vmm.BootSpec
type NetIface = vmm.NetIface

// Manager tracks the QEMU processes this compute-agent has booted. One
// Manager per compute-agent process.
type Manager struct {
	// BinPath is the qemu-system binary to exec. Defaults to
	// "qemu-system-x86_64" (resolved via $PATH) if empty.
	BinPath string
	// CacheDir holds downloaded kernel/rootfs artifacts, keyed by a hash of
	// their URL -- shared read-only across all VMs booted from the same
	// Image. Kept separate from fcvmm's own cache dir (not because the
	// artifacts differ -- a KERNEL_ROOTFS Image is identical either way --
	// but so each driver's on-disk state stays independently inspectable.
	// Defaults to /var/lib/kyuusha/qemu-cache.
	CacheDir string
	// RunDir holds one subdirectory per running VM (its writable rootfs
	// copy, console log, and cloud-init seed disk if any). Defaults to
	// /var/lib/kyuusha/qemu-run.
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
// here (unlike fcvmm's runningVM): QEMU opens a Volume's already-visible
// device/file path directly via -drive, with no jail to place it into and
// so nothing of this driver's own left to unwind at teardown.
type runningVM struct {
	cmd  *exec.Cmd
	taps []string
}

func (m *Manager) binPath() string {
	if m.BinPath != "" {
		return m.BinPath
	}
	return "qemu-system-x86_64"
}

func (m *Manager) cacheDir() string {
	if m.CacheDir != "" {
		return m.CacheDir
	}
	return "/var/lib/kyuusha/qemu-cache"
}

func (m *Manager) runDir() string {
	if m.RunDir != "" {
		return m.RunDir
	}
	return "/var/lib/kyuusha/qemu-run"
}

// ConsoleLogPath is where Boot(vmID's spec) captures QEMU's stdout/stderr
// (== the guest's serial console, ttyS0, via -serial stdio below) -- see
// docs/specs/qemu-boot.md. It exists only once Boot has actually run for
// this vmID.
func (m *Manager) ConsoleLogPath(vmID string) string {
	return filepath.Join(m.runDir(), vmID, "console.log")
}

// Boot fetches (or reuses cached copies of) spec's kernel/rootfs, gives the
// VM its own writable rootfs copy and run directory, and starts QEMU
// against them. It returns once QEMU has either exited immediately (an
// error) or stayed up past bootGracePeriod (success) -- see that
// constant's doc for exactly what "success" does and doesn't mean.
func (m *Manager) Boot(ctx context.Context, spec BootSpec) error {
	if err := os.MkdirAll(m.cacheDir(), 0o755); err != nil {
		return fmt.Errorf("qemuvmm: create cache dir: %w", err)
	}
	kernelPath, err := m.ensureCached(ctx, spec.KernelURL)
	if err != nil {
		return fmt.Errorf("qemuvmm: fetch kernel: %w", err)
	}
	masterRootfs, err := m.ensureCached(ctx, spec.RootfsURL)
	if err != nil {
		return fmt.Errorf("qemuvmm: fetch rootfs: %w", err)
	}

	vmDir := filepath.Join(m.runDir(), spec.VMID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("qemuvmm: create run dir: %w", err)
	}

	// QEMU's virtio-blk backend opens the drive read-write and writes guest
	// changes straight into the backing file, so every VM needs its own
	// copy -- the cached master is shared read-only across VMs booted from
	// the same Image.
	rootfsCopy := filepath.Join(vmDir, "rootfs.raw")
	if err := copyFile(masterRootfs, rootfsCopy); err != nil {
		return fmt.Errorf("qemuvmm: copy rootfs: %w", err)
	}

	// Wire every real network interface before QEMU starts (it opens each
	// tap by name at boot, so it must already exist) -- same
	// internal/compute-agent/netsetup this VM's counterpart under fcvmm
	// uses; a tap device works identically regardless of which VMM process
	// ends up attached to it. On a failure partway through, unwire whatever
	// this call already created rather than leaking tap devices for a VM
	// that never boots.
	var netArgs []string
	var qemuNetArgs []string
	var taps []string
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
			return fmt.Errorf("qemuvmm: wire network interface %d (%s): %w", i, ni.IfaceID, err)
		}
		taps = append(taps, wired.TapName)
		netdevID := fmt.Sprintf("net%d", i)
		qemuNetArgs = append(qemuNetArgs,
			"-netdev", fmt.Sprintf("tap,id=%s,ifname=%s,script=no,downscript=no", netdevID, wired.TapName),
			"-device", fmt.Sprintf("virtio-net-pci,netdev=%s,mac=%s", netdevID, wired.MACAddress),
		)
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

	driveArgs := []string{"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", rootfsCopy)}
	if spec.UserData != "" {
		seedImg, err := vmm.BuildSeedDisk(vmDir, spec.VMID, spec.UserData, spec.NetworkInterfaces)
		if err != nil {
			cleanup()
			return fmt.Errorf("qemuvmm: build seed disk: %w", err)
		}
		// Read-only, non-root: the guest sees this as a second virtio-blk
		// device (typically /dev/vdb) alongside its root disk, exactly what
		// cloud-init's NoCloud datasource expects -- same as fcvmm's seed
		// drive.
		driveArgs = append(driveArgs, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,readonly=on", seedImg))
	}

	// Discover every already-visible Volume before QEMU starts -- same
	// reasoning as taps, see internal/compute-agent/volumeref and
	// docs/specs/volume.md. Each becomes its own read-write virtio-blk
	// drive alongside rootfs/seed, referenced by its real path directly:
	// unlike fcvmm, there's no jail here for the path to need placing into.
	for i, v := range spec.Volumes {
		devPath, sizeBytes, err := volumeref.Resolve(m.StorageConnections, v.Protocol, v.StorageConnection, v.Identifier)
		if err != nil {
			cleanup()
			return fmt.Errorf("qemuvmm: resolve volume %d (%s): %w", i, v.AttachmentID, err)
		}
		vmm.WarnIfSizeMismatch(v, sizeBytes)
		driveArgs = append(driveArgs, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", devPath))
	}

	consoleLog, err := os.Create(filepath.Join(vmDir, "console.log"))
	if err != nil {
		cleanup()
		return fmt.Errorf("qemuvmm: create console log: %w", err)
	}
	defer consoleLog.Close()

	args := []string{
		// q35: a real PCIe machine, deliberately not the minimal `microvm`
		// type QEMU also offers -- microvm has no PCI bus at all, which
		// would rule out VFIO/PCI passthrough later (see package doc).
		"-M", "q35",
		"-enable-kvm",
		"-cpu", "host",
		"-smp", fmt.Sprintf("%d", spec.VCPU),
		"-m", fmt.Sprintf("%dM", spec.MemoryMB),
		"-kernel", kernelPath,
		"-append", bootArgs,
		// No firmware/BIOS boot chain, no monitor, no default devices
		// (default VGA/parallel/floppy/etc.) -- ttyS0 is the only I/O
		// surface, exactly like fcvmm's console.log contract: QEMU's own
		// stdout/stderr (below) becomes both the guest serial console and
		// wherever QEMU's own startup errors land, in one file.
		"-display", "none",
		"-serial", "stdio",
		"-monitor", "none",
		"-nodefaults",
	}
	args = append(args, driveArgs...)
	args = append(args, qemuNetArgs...)

	// Not exec.CommandContext(ctx, ...): ctx here is the NATS message
	// handler's context, which is done long before this VM's guest is --
	// the process's lifetime is managed explicitly via m.running/Stop (same
	// reasoning as fcvmm).
	cmd := exec.Command(m.binPath(), args...)
	cmd.Stdout = consoleLog
	cmd.Stderr = consoleLog
	if err := cmd.Start(); err != nil {
		cleanup()
		return fmt.Errorf("qemuvmm: start qemu: %w", err)
	}

	// Best-effort: a host/container without usable cgroup v2 delegation just
	// boots this VM unconstrained -- see internal/compute-agent/cgroup's doc
	// comment.
	if err := cgroup.Apply(spec.VMID, spec.VCPU, spec.MemoryMB, cmd.Process.Pid); err != nil {
		slog.Warn("qemuvmm: cgroup limits not applied, VM will boot unconstrained", "vm_id", spec.VMID, "err", err)
	}

	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	select {
	case err := <-exitCh:
		cleanup()
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("qemuvmm: removing cgroup after immediate exit", "vm_id", spec.VMID, "err", rmErr)
		}
		return fmt.Errorf("qemuvmm: qemu exited immediately (see %s): %w", consoleLog.Name(), err)
	case <-time.After(bootGracePeriod):
	}

	m.mu.Lock()
	if m.running == nil {
		m.running = make(map[string]*runningVM)
	}
	m.running[spec.VMID] = &runningVM{cmd: cmd, taps: taps}
	m.mu.Unlock()

	go func() {
		err := <-exitCh
		m.mu.Lock()
		delete(m.running, spec.VMID)
		m.mu.Unlock()
		cleanup()
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("qemuvmm: removing cgroup", "vm_id", spec.VMID, "err", rmErr)
		}
		if err != nil {
			slog.Warn("qemuvmm: qemu process exited", "vm_id", spec.VMID, "err", err)
		} else {
			slog.Info("qemuvmm: qemu process exited", "vm_id", spec.VMID)
		}
	}()

	return nil
}

// Stop tears down vmID's QEMU process if one is running. A no-op if this
// compute-agent never booted a real process for it (a VM some other driver
// booted, or one that already exited) -- deletion isn't gated on this, see
// reconciler.go's releaseIfReserved.
func (m *Manager) Stop(vmID string) {
	m.mu.Lock()
	rv, ok := m.running[vmID]
	m.mu.Unlock()
	if !ok {
		return
	}
	_ = rv.cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		time.Sleep(killGracePeriod)
		_ = rv.cmd.Process.Signal(syscall.SIGKILL) // no-op if it already exited on SIGTERM
	}()
	// Tap cleanup happens in Boot's exit-watch goroutine once the process
	// actually exits, not here -- same reasoning as fcvmm.
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
