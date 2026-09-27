package fcvmm

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/imagestore"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
)

// TestManagerReconcileAdoptsRunningProcessAcrossRestart exercises the exact
// mechanism a compute-agent binary upgrade (in-place process restart in
// the same host/network namespace -- see docs/specs/snap.md on why this
// is different from a Docker container recreate, and docs/rolling-
// upgrade.md for the procedure that depends on it) needs for already-
// running VMs to survive: a brand new *Manager, constructed the same way
// cmd/compute-agent/main.go would after a restart, with no in-memory
// knowledge of anything this process itself booted, must still find and
// adopt a VM whose process is still alive from a *previous* Manager's own
// Boot. This had zero test coverage at any level until now -- Reconcile's
// own doc comment names exactly the failure mode a bug here would cause
// ("a restarted compute-agent would silently forget every VM it had
// previously booted... Stop/Destroy would no-op for them forever").
//
// Stands in a real long-lived process (`sleep`, not an actual Firecracker/
// jailer child, which would need real KVM access and root) for "the VM
// this Manager is adopting" -- Reconcile only cares about a BootRecord's
// own pid/exe_basename matching a real, still-running process, not what
// that process actually is.
func TestManagerReconcileAdoptsRunningProcessAcrossRestart(t *testing.T) {
	runDir := t.TempDir()
	const vmID = "vm-adopt-test"
	vmDir := filepath.Join(runDir, vmID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatalf("mkdir vmDir: %v", err)
	}

	standIn := exec.Command("sleep", "300")
	if err := standIn.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	t.Cleanup(func() { _ = standIn.Process.Kill() })

	if err := vmm.WriteBootRecord(vmDir, vmm.BootRecord{
		PID:         standIn.Process.Pid,
		ExeBasename: "sleep",
		TenantID:    "tenant-a",
		Taps:        []string{"tap-adopt-test"},
		PinnedKeys:  []string{"sha256:deadbeef"},
	}); err != nil {
		t.Fatalf("WriteBootRecord: %v", err)
	}

	// A brand new Manager, exactly as cmd/compute-agent/main.go would
	// construct after a restart: no in-memory knowledge of vm-adopt-test
	// at all, only the same on-disk RunDir a previous process left behind.
	m := &Manager{RunDir: runDir, ImageStore: &imagestore.Store{}}
	m.Reconcile()

	running := m.Running()
	if len(running) != 1 {
		t.Fatalf("Running() = %d entries, want 1 (the adopted VM): %+v", len(running), running)
	}
	rv := running[0]
	if rv.VMID != vmID {
		t.Fatalf("adopted VMID = %q, want %q", rv.VMID, vmID)
	}
	if rv.TenantID != "tenant-a" {
		t.Fatalf("adopted TenantID = %q, want %q", rv.TenantID, "tenant-a")
	}
	if rv.PID != standIn.Process.Pid {
		t.Fatalf("adopted PID = %d, want %d", rv.PID, standIn.Process.Pid)
	}

	if err := standIn.Process.Kill(); err != nil {
		t.Fatalf("kill stand-in process: %v", err)
	}
	_ = standIn.Wait()

	// watchAdopted polls every adoptedPollInterval (2s), then tears the
	// adoption down in several steps -- removing m.running first, the boot
	// record last (see its own goroutine's ordering) -- so this must poll
	// for both together, not treat "Running() is empty" as proof the boot
	// record is already gone too.
	deadline := time.Now().Add(8 * time.Second)
	for {
		_, recordErr := vmm.ReadBootRecord(vmDir)
		if len(m.Running()) == 0 && recordErr != nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("teardown after the adopted process exited did not complete in time: Running()=%d, boot record read err=%v", len(m.Running()), recordErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
