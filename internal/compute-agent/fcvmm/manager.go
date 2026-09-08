// Package fcvmm runs real Firecracker microVMs for compute-agent. This is
// the first real (non-stub) VMM integration: it deliberately skips jailer's
// chroot/namespace/uid-drop process isolation -- see
// docs/specs/firecracker-boot.md for the scope this covers and what it
// doesn't. It does apply host-side cgroup v2 CPU/memory limits (see
// internal/compute-agent/cgroup) derived directly from spec.vcpu/
// spec.memory_mb, best-effort: a host/container without usable cgroup v2
// delegation just boots the VM unconstrained, same as before this existed.
// Real network interfaces
// (tap devices, per-VLAN bridges -- see internal/compute-agent/netsetup)
// are wired for VMs whose spec carries them; a VM with none boots exactly
// as before (network-less, serial-only). A VM with spec.user_data set gets
// a cloud-init NoCloud seed disk (see seed.go and docs/architecture.md
// "UserData注入: NoCloud seed disk"). Only driver_hint=FIRECRACKER
// (KERNEL_ROOTFS images) is handled here; QEMU remains an unimplemented
// stub, same as before this package existed.
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

	"gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent/cgroup"
	"gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent/netsetup"
)

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

// BootSpec is what Manager needs to boot one VM. Callers (agent.go) build
// this from a compute.CreateCommand.
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
	// RunDir holds one subdirectory per running VM (its writable rootfs
	// copy, API socket, config, and console log). Defaults to
	// /var/lib/kyuusha/fc-run.
	RunDir string

	downloadMu sync.Mutex // serializes ensureCached; fine at playground scale

	mu      sync.Mutex
	running map[string]*runningVM
}

// runningVM tracks what Boot did for one VM, so Stop (and the exit-watch
// goroutine Boot starts) can tear all of it down: the process itself, and
// every tap device Wire created for it.
type runningVM struct {
	cmd  *exec.Cmd
	taps []string
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
func (m *Manager) Boot(ctx context.Context, spec BootSpec) error {
	if err := os.MkdirAll(m.cacheDir(), 0o755); err != nil {
		return fmt.Errorf("fcvmm: create cache dir: %w", err)
	}
	kernelPath, err := m.ensureCached(ctx, spec.KernelURL)
	if err != nil {
		return fmt.Errorf("fcvmm: fetch kernel: %w", err)
	}
	masterRootfs, err := m.ensureCached(ctx, spec.RootfsURL)
	if err != nil {
		return fmt.Errorf("fcvmm: fetch rootfs: %w", err)
	}

	vmDir := filepath.Join(m.runDir(), spec.VMID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("fcvmm: create run dir: %w", err)
	}

	// Firecracker opens its root drive read-write and writes guest changes
	// straight into the backing file, so every VM needs its own copy -- the
	// cached master is shared read-only across VMs booted from the same
	// Image.
	rootfsCopy := filepath.Join(vmDir, "rootfs.ext4")
	if err := copyFile(masterRootfs, rootfsCopy); err != nil {
		return fmt.Errorf("fcvmm: copy rootfs: %w", err)
	}

	apiSock := filepath.Join(vmDir, "api.sock")
	_ = os.Remove(apiSock) // stale socket from a previous failed attempt, if any; Firecracker refuses to start if this exists

	// Wire every real network interface before Firecracker starts (it opens
	// each host_dev_name by name at boot, so the tap must already exist).
	// On a failure partway through, unwire whatever this call already
	// created rather than leaking tap devices for a VM that never boots.
	var fcNetIfaces []fcNetworkInterface
	var netArgs []string
	var taps []string
	for i, ni := range spec.NetworkInterfaces {
		wired, err := netsetup.Wire(netsetup.Interface{
			IfaceID:    ni.IfaceID,
			MACAddress: ni.MACAddress,
			GatewayIP:  ni.GatewayIP,
			PrefixLen:  ni.PrefixLen,
			VLANID:     ni.VLANID,
		})
		if err != nil {
			for _, t := range taps {
				_ = netsetup.DeleteTap(t)
			}
			return fmt.Errorf("fcvmm: wire network interface %d (%s): %w", i, ni.IfaceID, err)
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

	drives := []fcDrive{{
		DriveID:      "rootfs",
		PathOnHost:   rootfsCopy,
		IsRootDevice: true,
		IsReadOnly:   false,
	}}
	if spec.UserData != "" {
		seedISO, err := buildSeedDisk(vmDir, spec.VMID, spec.UserData, spec.NetworkInterfaces)
		if err != nil {
			for _, t := range taps {
				_ = netsetup.DeleteTap(t)
			}
			return fmt.Errorf("fcvmm: build seed disk: %w", err)
		}
		// Read-only, non-root: the guest sees this as a second
		// virtio-block device (typically /dev/vdb) alongside its root
		// disk, exactly what cloud-init's NoCloud datasource expects.
		drives = append(drives, fcDrive{
			DriveID:      "seed",
			PathOnHost:   seedISO,
			IsRootDevice: false,
			IsReadOnly:   true,
		})
	}

	cfg := fcConfig{
		BootSource:        fcBootSource{KernelImagePath: kernelPath, BootArgs: bootArgs},
		Drives:            drives,
		MachineConfig:     fcMachineConfig{VcpuCount: spec.VCPU, MemSizeMib: spec.MemoryMB},
		NetworkInterfaces: fcNetIfaces,
	}
	configPath := filepath.Join(vmDir, "config.json")
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
		return fmt.Errorf("fcvmm: marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o644); err != nil {
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
		return fmt.Errorf("fcvmm: write config: %w", err)
	}

	consoleLog, err := os.Create(filepath.Join(vmDir, "console.log"))
	if err != nil {
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
		return fmt.Errorf("fcvmm: create console log: %w", err)
	}
	defer consoleLog.Close()

	// Not exec.CommandContext(ctx, ...): ctx here is the NATS message
	// handler's context, which is done long before this VM's guest is --
	// the process's lifetime is managed explicitly via m.running/Stop.
	cmd := exec.Command(m.binPath(), "--api-sock", apiSock, "--config-file", configPath)
	cmd.Stdout = consoleLog
	cmd.Stderr = consoleLog
	if err := cmd.Start(); err != nil {
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
		return fmt.Errorf("fcvmm: start firecracker: %w", err)
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
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("fcvmm: removing cgroup after immediate exit", "vm_id", spec.VMID, "err", rmErr)
		}
		return fmt.Errorf("fcvmm: firecracker exited immediately (see %s): %w", consoleLog.Name(), err)
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
		for _, t := range taps {
			_ = netsetup.DeleteTap(t)
		}
		if rmErr := cgroup.Remove(spec.VMID); rmErr != nil {
			slog.Warn("fcvmm: removing cgroup", "vm_id", spec.VMID, "err", rmErr)
		}
		if err != nil {
			slog.Warn("fcvmm: firecracker process exited", "vm_id", spec.VMID, "err", err)
		} else {
			slog.Info("fcvmm: firecracker process exited", "vm_id", spec.VMID)
		}
	}()

	return nil
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
