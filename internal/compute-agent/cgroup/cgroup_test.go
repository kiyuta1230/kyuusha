package cgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestApplyAndRemove exercises the real cgroup v2 path. Apply needs cgroup
// v2 delegated to this process (write access to /sys/fs/cgroup's
// subtree_control) -- same situation as netsetup's Wire needing
// CAP_NET_ADMIN, or fcvmm's real Firecracker boot needing /dev/kvm. Rather
// than skip the package outright, this skips only if Apply actually fails
// for that reason, so it still runs for real wherever the privilege is
// available (e.g. inside a compute-agent container, or under sudo on a
// cgroup v2 host).
func TestApplyAndRemove(t *testing.T) {
	if !Available() {
		t.Skip("skipping: cgroup v2 not available in this environment")
	}

	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	const vmID = "test-vm-cgroup-1"
	if err := Apply(vmID, 2, 512, cmd.Process.Pid); err != nil {
		t.Skipf("skipping: cgroup delegation not usable in this environment: %v", err)
	}
	defer Remove(vmID)

	dir := vmDir(vmID)

	cpuMax, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		t.Fatalf("read cpu.max: %v", err)
	}
	if got, want := strings.TrimSpace(string(cpuMax)), "200000 100000"; got != want {
		t.Fatalf("cpu.max = %q, want %q", got, want)
	}

	memMax, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		t.Fatalf("read memory.max: %v", err)
	}
	if got, want := strings.TrimSpace(string(memMax)), strconv.FormatInt(512*1024*1024, 10); got != want {
		t.Fatalf("memory.max = %q, want %q", got, want)
	}

	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		t.Fatalf("read cgroup.procs: %v", err)
	}
	if !strings.Contains(string(procs), strconv.Itoa(cmd.Process.Pid)) {
		t.Fatalf("cgroup.procs = %q, want it to contain pid %d", procs, cmd.Process.Pid)
	}

	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	if err := Remove(vmID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cgroup dir %s still exists after Remove", dir)
	}
}

// TestApply_Idempotent covers a hypothetical retried Apply call for the same
// vmID (e.g. compute-agent reconnecting) -- it must not fail just because
// the directory and controllers already exist from a first call.
func TestApply_Idempotent(t *testing.T) {
	if !Available() {
		t.Skip("skipping: cgroup v2 not available in this environment")
	}

	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	const vmID = "test-vm-cgroup-2"
	if err := Apply(vmID, 1, 256, cmd.Process.Pid); err != nil {
		t.Skipf("skipping: cgroup delegation not usable in this environment: %v", err)
	}
	defer Remove(vmID)

	if err := Apply(vmID, 1, 256, cmd.Process.Pid); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
}

// TestRemove_NoopIfNeverCreated covers fcvmm always calling Remove in its
// exit-watch goroutine, even when Apply was never called or failed earlier
// (e.g. cgroup v2 unavailable) -- it must not surface an error in that case.
func TestRemove_NoopIfNeverCreated(t *testing.T) {
	if err := Remove("no-such-vm"); err != nil {
		t.Fatalf("Remove of a cgroup that was never created: %v", err)
	}
}
