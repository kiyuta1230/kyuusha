package vmm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// BootRecordFileName is the JSON file each VMM driver writes into a VM's
// run directory once Boot succeeds, and removes once the process exits.
// It's the only state that survives a compute-agent restart: each
// driver's own in-memory "which VMs are running" map starts empty every
// time the process starts, but the VMM process itself is very likely
// still alive on a real hypervisor host -- compute-agent isn't its parent
// in any way that ties their lifetimes together (jailer/the VMM binary
// exec's directly, no shared PID namespace), and compute-agent is deployed
// one per hypervisor host, not as a container's PID 1 whose death takes
// its children with it (see docs/architecture.md "各ハイパーバイザーに配置").
// Without this, a restarted compute-agent would (a) silently no-op every
// Stop/Destroy call against a VM it forgot about, forever, and (b) have no
// way to refuse a duplicate Boot request for the same vm_id, risking a
// second, genuinely-duplicate process wiping out the first one's jail out
// from under it.
const BootRecordFileName = "kyuusha-boot.json"

// BootRecord is one VMM driver's account of what it did for one VM, as of
// its last successful Boot -- enough for Reconcile to adopt it (Taps/
// VolumeMounts so eventual teardown still unwinds them; Attached so an
// idempotent re-Boot can return the exact same result agent.go already
// reported to block-storage).
type BootRecord struct {
	PID         int    `json:"pid"`
	ExeBasename string `json:"exe_basename"`
	// TenantID is this VM's owning tenant (BootSpec.TenantID at Boot time),
	// persisted so Reconcile can still label an adopted VM correctly for
	// /metrics/resources (internal/compute-agent/resourcemetrics) even
	// though the compute-agent process reading it back otherwise has no
	// memory of this VM at all.
	TenantID     string           `json:"tenant_id,omitempty"`
	Taps         []string         `json:"taps,omitempty"`
	VolumeMounts []string         `json:"volume_mounts,omitempty"`
	Attached     []AttachedVolume `json:"attached,omitempty"`
}

func bootRecordPath(vmDir string) string {
	return filepath.Join(vmDir, BootRecordFileName)
}

// WriteBootRecord persists rec for vmDir.
func WriteBootRecord(vmDir string, rec BootRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("vmm: marshal boot record: %w", err)
	}
	return os.WriteFile(bootRecordPath(vmDir), data, 0o644)
}

// ReadBootRecord reads back what WriteBootRecord wrote for vmDir. An error
// (including "no such file", the common case for a vmDir that never held a
// real process, or one whose record was already removed) means there's
// nothing to adopt.
func ReadBootRecord(vmDir string) (BootRecord, error) {
	var rec BootRecord
	data, err := os.ReadFile(bootRecordPath(vmDir))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("vmm: unmarshal boot record: %w", err)
	}
	return rec, nil
}

// RemoveBootRecord deletes vmDir's boot record, if any -- called once a
// VM's process is confirmed gone (the normal exit-watch goroutine, or an
// adopted VM's poll loop), so a stale record never outlives the process it
// describes.
func RemoveBootRecord(vmDir string) {
	_ = os.Remove(bootRecordPath(vmDir))
}

// ProcessAlive reports whether pid is a live process. When exeBasename is
// non-empty, it's also checked against /proc/<pid>/exe's target -- a cheap
// guard against pid having been recycled by an unrelated process since it
// was recorded; a plain "does this pid exist" check alone can't tell those
// apart.
func ProcessAlive(pid int, exeBasename string) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if exeBasename == "" {
		return true
	}
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false
	}
	return filepath.Base(target) == exeBasename
}
