// Package fcvmm runs real Firecracker microVMs for compute-agent, always
// via jailer -- see docs/specs/firecracker-boot.md "jailer" for the scope
// this covers and what it doesn't (network/PID namespace isolation and
// per-VM unique uid/gid are deliberately deferred; every VM's jail shares
// one fixed uid/gid). It does apply host-side cgroup v2 CPU/memory limits
// (see internal/compute-agent/cgroup) derived directly from spec.vcpu/
// spec.memory_mb, best-effort: a host/container without usable cgroup v2
// delegation just boots the VM unconstrained, same as before this existed.
// Manager implements internal/compute-agent/vmm.VMM -- see qemuvmm for the
// other implementation (driver_hint=QEMU). Real network interfaces
// (tap devices, per-VLAN bridges -- see internal/compute-agent/netsetup)
// are wired for VMs whose spec carries them; a VM with none boots exactly
// as before (network-less, serial-only). A VM with spec.user_data set gets
// a cloud-init NoCloud seed disk (see internal/compute-agent/vmm's
// BuildSeedDisk and docs/architecture.md "UserData注入: NoCloud seed
// disk"). Handles driver_hint=FIRECRACKER only; see
// internal/compute-agent/qemuvmm for driver_hint=QEMU, which boots from the
// exact same kind of Image (KERNEL_ROOTFS: a kernel + a raw rootfs, no
// bootloader) via a different VMM process.
package fcvmm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"golang.org/x/sys/unix"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/netsetup"
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
)

// BootSpec and NetIface are aliases (not new types) for
// internal/compute-agent/vmm's shapes: Manager implements vmm.VMM, and
// agent.go builds one shared vmm.BootSpec value regardless of which
// driver_hint it's dispatching to, so both must be the exact same type as
// what qemuvmm.Manager.Boot accepts, not merely structurally similar
// copies.
type BootSpec = vmm.BootSpec
type NetIface = vmm.NetIface

// Manager tracks the Firecracker processes this compute-agent has booted.
// One Manager per compute-agent process.
type Manager struct {
	// BinPath is the firecracker binary to exec. Defaults to "firecracker"
	// (resolved via $PATH) if empty.
	BinPath string
	// CacheDir holds downloaded kernel/rootfs artifacts, keyed by a hash of
	// their URL -- shared read-only across all VMs booted from the same
	// Image. Defaults to /var/lib/kyuusha/fc-cache.
	CacheDir string
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

	downloadMu sync.Mutex // serializes ensureCached; fine at playground scale

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
	cmd          *exec.Cmd
	taps         []string
	volumeMounts []string
}

func (m *Manager) binPath() string {
	if m.BinPath != "" {
		return m.BinPath
	}
	return "firecracker"
}

func (m *Manager) cacheDir() string {
	if m.CacheDir != "" {
		return m.CacheDir
	}
	return "/var/lib/kyuusha/fc-cache"
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

// ConsoleLogPath is where Boot(vmID's spec) captures Firecracker's stdout/
// stderr (== the guest's serial console, ttyS0) -- see docs/specs/
// firecracker-boot.md. It exists only once Boot has actually run for this
// vmID (never, for a stub-succeeded QEMU VM or one that hasn't booted yet).
func (m *Manager) ConsoleLogPath(vmID string) string {
	return filepath.Join(m.runDir(), vmID, "console.log")
}

// Boot fetches (or reuses cached copies of) spec's kernel/rootfs, gives the
// VM its own writable rootfs copy and run directory, and starts Firecracker
// against them. It returns once Firecracker has either exited immediately
// (an error) or stayed up past bootGracePeriod (success) -- see that
// constant's doc for exactly what "success" does and doesn't mean.
func (m *Manager) Boot(ctx context.Context, spec BootSpec) ([]vmm.AttachedVolume, error) {
	if err := os.MkdirAll(m.cacheDir(), 0o755); err != nil {
		return nil, fmt.Errorf("fcvmm: create cache dir: %w", err)
	}
	kernelPath, err := m.ensureCached(ctx, spec.KernelURL)
	if err != nil {
		return nil, fmt.Errorf("fcvmm: fetch kernel: %w", err)
	}
	masterRootfs, err := m.ensureCached(ctx, spec.RootfsURL)
	if err != nil {
		return nil, fmt.Errorf("fcvmm: fetch rootfs: %w", err)
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
	if err := os.MkdirAll(chroot, 0o755); err != nil {
		return nil, fmt.Errorf("fcvmm: create jail chroot dir: %w", err)
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
	// Image.
	rootfsCopy := filepath.Join(chroot, "rootfs.ext4")
	if err := placeWritableResource(masterRootfs, rootfsCopy, jailUID, jailGID); err != nil {
		return nil, fmt.Errorf("fcvmm: copy rootfs into jail: %w", err)
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
	var volumeMounts []string
	var attached []vmm.AttachedVolume
	cleanup := func() {
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
		for _, p := range volumeMounts {
			_ = unix.Unmount(p, 0)
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
			return nil, fmt.Errorf("fcvmm: wire network interface %d (%s): %w", i, ni.IfaceID, err)
		}
		taps = append(taps, wired.TapName)
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
	if err := cgroup.Apply(spec.VMID, spec.VCPU, spec.MemoryMB, cmd.Process.Pid); err != nil {
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

	m.mu.Lock()
	if m.running == nil {
		m.running = make(map[string]*runningVM)
	}
	m.running[spec.VMID] = &runningVM{cmd: cmd, taps: taps, volumeMounts: volumeMounts}
	m.mu.Unlock()

	go func() {
		err := <-exitCh
		m.mu.Lock()
		delete(m.running, spec.VMID)
		m.mu.Unlock()
		cleanup()
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("fcvmm: removing cgroup", "vm_id", spec.VMID, "err", rmErr)
		}
		if err != nil {
			slog.Warn("fcvmm: firecracker process exited", "vm_id", spec.VMID, "err", err)
		} else {
			slog.Info("fcvmm: firecracker process exited", "vm_id", spec.VMID)
		}
	}()

	return attached, nil
}

// Stop tears down vmID's Firecracker process if one is running. A no-op if
// this compute-agent never booted a real process for it (stub-succeeded
// path, or it already exited) -- deletion isn't gated on this, see
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
	// actually exits, not here -- deleting a tap while Firecracker still
	// has it open is unnecessary churn for no benefit.
}

// ensureCached downloads rawURL into CacheDir if not already present,
// keyed by a hash of the URL itself (not its content -- Image artifacts
// aren't required to carry a digest; see docs/specs/image.md). Concurrent
// callers for the same or different URLs are serialized by downloadMu,
// which is fine at this system's target scale.
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
