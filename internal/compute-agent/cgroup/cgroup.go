// Package cgroup applies best-effort cgroup v2 CPU/memory limits to a
// Firecracker VMM process, derived directly from spec.vcpu/spec.memory_mb
// (docs/architecture.md "Firecracker: jailerとtapデバイス"). It is
// deliberately scoped down from jailer: it does not chroot, does not create
// a new mount/network/PID namespace, and does not drop privileges. It only
// enforces the two resource numbers Firecracker was already given as the
// guest's virtual topology as a host-side ceiling too. See
// docs/specs/firecracker-boot.md "この実装がカバーしないもの" for what
// remains unimplemented.
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// root is the cgroupfs mount point. A package var (not a const) so tests can
// point it at a scratch directory instead of the real /sys/fs/cgroup.
var root = "/sys/fs/cgroup"

func parentDir() string        { return filepath.Join(root, "kyuusha") }
func vmDir(vmID string) string { return filepath.Join(parentDir(), vmID) }

// Available reports whether cgroup v2 (the unified hierarchy) is mounted and
// usable from this process. False on a cgroup v1 host, or one where
// cgroupfs isn't delegated to this container -- callers should treat that
// as "boot unconstrained", not an error.
func Available() bool {
	_, err := os.Stat(filepath.Join(root, "cgroup.controllers"))
	return err == nil
}

// Apply creates (or reuses) vmID's cgroup, sets cpu.max/memory.max from
// vcpu/memoryMB, and moves pid into it. vcpu becomes a hard CPU quota of
// vcpu full cores (a <n>*100000us quota per 100000us period) -- the same
// number Firecracker was given as the guest's vcpu_count, now also an
// enforced host-side ceiling. memoryMB becomes memory.max in bytes.
//
// Callers should treat a non-nil error as best-effort-failed (log a warning
// and let the VM boot unconstrained) rather than fatal: cgroup v2
// availability and delegation vary across hosts/containers, and this is
// resource *limiting* on top of an already-working VM, not isolation
// jailer would additionally provide.
func Apply(vmID string, vcpu int32, memoryMB int64, pid int) error {
	if !Available() {
		return fmt.Errorf("cgroup: cgroup v2 not available at %s", root)
	}
	if err := Init(); err != nil {
		return fmt.Errorf("cgroup: relocating own process out of %s: %w", root, err)
	}
	if err := enableControllers(root); err != nil {
		return fmt.Errorf("cgroup: enabling controllers on %s: %w", root, err)
	}
	if err := os.MkdirAll(parentDir(), 0o755); err != nil {
		return fmt.Errorf("cgroup: creating %s: %w", parentDir(), err)
	}
	if err := enableControllers(parentDir()); err != nil {
		return fmt.Errorf("cgroup: enabling controllers on %s: %w", parentDir(), err)
	}
	dir := vmDir(vmID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cgroup: creating %s: %w", dir, err)
	}

	quotaUS := int64(vcpu) * 100000
	cpuMax := fmt.Sprintf("%d 100000", quotaUS)
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(cpuMax), 0o644); err != nil {
		return fmt.Errorf("cgroup: writing cpu.max: %w", err)
	}

	memMax := strconv.FormatInt(memoryMB*1024*1024, 10)
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(memMax), 0o644); err != nil {
		return fmt.Errorf("cgroup: writing memory.max: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("cgroup: adding pid %d: %w", pid, err)
	}
	return nil
}

// Remove deletes vmID's cgroup. Call only after its process has actually
// been wait(2)ed on (fcvmm.Manager's exit-watch goroutine, after cmd.Wait()
// returns): a cgroup can't be removed while cgroup.procs is non-empty, which
// the kernel guarantees only once the process is fully reaped, not merely
// killed -- hence the retry loop. A no-op (nil error) if the cgroup was
// never created (Apply was never called, or it failed).
func Remove(vmID string) error {
	dir := vmDir(vmID)
	var lastErr error
	for range 20 {
		lastErr = os.Remove(dir)
		if lastErr == nil || os.IsNotExist(lastErr) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("cgroup: removing %s: %w", dir, lastErr)
}

// Init moves this process out of root's own cgroup.procs and into a leaf
// "init" child. cgroup v2's "no internal process" constraint forbids
// enabling subtree_control (i.e. delegating cpu/memory to children) on a
// cgroup that has member processes directly -- and a container's main
// process (this one, typically PID 1 in its container) starts out placed
// directly in its container cgroup, which is exactly what a
// cgroupns=private container sees as root here.
//
// Callers (cmd/compute-agent) should call this once at startup, before
// booting any VM: Apply also calls it (so it's safe even if a caller
// forgets), but by then it's too late for that VM's own Firecracker
// process -- exec.Command forks the child while this process is still
// wherever it currently resides, and a child already forked directly into
// root can't be un-forked into "init" after the fact. Only a child forked
// after this process has already relocated inherits "init" for free.
// Idempotent: re-adding a pid already in "init" is a harmless no-op.
func Init() error {
	initDir := filepath.Join(root, "init")
	if err := os.MkdirAll(initDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(initDir, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// enableControllers turns on cpu/memory for dir's children via
// subtree_control, for whichever of those two are actually listed in dir's
// own cgroup.controllers (a delegated subtree may not carry both). A no-op
// if they're already enabled.
func enableControllers(dir string) error {
	available, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return err
	}
	existing, _ := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))

	var need []string
	for _, c := range []string{"cpu", "memory"} {
		if strings.Contains(string(available), c) && !strings.Contains(string(existing), c) {
			need = append(need, "+"+c)
		}
	}
	if len(need) == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(strings.Join(need, " ")), 0o644)
}
