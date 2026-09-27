package chvmm

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/imagestore"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
)

// TestManagerReconcileAdoptsRunningProcessAcrossRestart is chvmm's copy of
// fcvmm's identical test -- see that one's doc comment for the full
// reasoning (docs/rolling-upgrade.md: this is the mechanism a compute-agent
// binary upgrade, an in-place process restart in the same host/network
// namespace, depends on for already-running VMs to survive). Both drivers
// implement Reconcile/watchAdopted near-identically but as separate code
// (see chvmm.Manager.Reconcile's own doc comment), so this had exactly the
// same zero-coverage gap fcvmm's did until now.
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
