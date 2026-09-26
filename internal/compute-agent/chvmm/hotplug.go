package chvmm

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/chapi"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
)

var _ vmm.Hotplugger = (*Manager)(nil)

const (
	// hotplugVCPUMultiplier/hotplugMemoryMultiplier: how far past a VM's
	// boot-time vcpu/memory a later LiveResize may go, expressed as a
	// multiple of the boot value -- not "as large as possible", because the
	// max=/hotplug_size= ceiling Boot declares isn't free: cloud-hypervisor
	// reserves real guest physical address space and a KVM memory-slot/ACPI
	// bookkeeping entry for it up front, shared with the finite set of
	// virtio-blk/net device BARs a VM can have (see docs/specs/
	// cloud-hypervisor-boot.md「--api-socket」). A flat multiplier keeps this
	// bounded and predictable per VM rather than reserving an arbitrary
	// amount "just in case" -- this is a policy call more than a hard
	// requirement, and the one number in this file most likely to get
	// revisited once real live-resize usage exists.
	hotplugVCPUMultiplier = 2
	// hotplugVCPUCeiling matches firecracker's own vcpu ceiling
	// (internal/compute/virtualmachine.go's firecrackerMaxVCPU) for
	// cross-driver consistency -- not something cloud-hypervisor itself
	// requires.
	hotplugVCPUCeiling = 32

	hotplugMemoryMultiplier = 2
	// hotplugMemoryCeilingAbsoluteMB is a flat safety cap regardless of boot
	// size, so a very large boot request doesn't reserve an equally large
	// hotplug window by the multiplier alone.
	hotplugMemoryCeilingAbsoluteMB = 262144 // 256GiB
)

// hotplugMaxVCPU is the --cpus max= value Boot declares for a VM booted
// with bootVCPU vcpus -- see the multiplier/ceiling constants above.
func hotplugMaxVCPU(bootVCPU int32) int32 {
	max := bootVCPU * hotplugVCPUMultiplier
	if max > hotplugVCPUCeiling {
		max = hotplugVCPUCeiling
	}
	if max < bootVCPU {
		max = bootVCPU
	}
	return max
}

// hotplugMemoryCeilingMB is the --memory hotplug_size= value Boot declares
// for a VM booted with bootMemoryMB of memory -- see the multiplier/ceiling
// constants above.
func hotplugMemoryCeilingMB(bootMemoryMB int64) int64 {
	max := bootMemoryMB * hotplugMemoryMultiplier
	if max > hotplugMemoryCeilingAbsoluteMB {
		max = hotplugMemoryCeilingAbsoluteMB
	}
	if max < bootMemoryMB {
		max = bootMemoryMB
	}
	return max
}

// apiSocketPath is where Boot's --api-socket for vmID lives -- a pure
// function of vmID, same pattern as ConsoleLogPath, so no runningVM field
// is needed to recover it for a live hotplug call.
func (m *Manager) apiSocketPath(vmID string) string {
	return filepath.Join(m.runDir(), vmID, "api.sock")
}

// client returns a chapi.Client for vmID, failing if this Manager doesn't
// currently have it running (a hotplug call for a VM this driver never
// booted, or one that has since exited, would otherwise dial a socket that
// either never existed or no longer has anyone listening).
func (m *Manager) client(vmID string) (*chapi.Client, error) {
	m.mu.Lock()
	_, ok := m.running[vmID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("chvmm: vm_id %q is not running under this driver", vmID)
	}
	return chapi.New(m.apiSocketPath(vmID)), nil
}

// LiveResize implements vmm.Hotplugger.
func (m *Manager) LiveResize(ctx context.Context, vmID string, vcpu int32, memoryMB int64) error {
	c, err := m.client(vmID)
	if err != nil {
		return err
	}
	if err := c.Resize(ctx, chapi.VmResize{DesiredVcpus: vcpu, DesiredRAM: memoryMB * 1024 * 1024}); err != nil {
		return err
	}

	// Best-effort, same as Boot's own cgroup.Apply call site: a failed
	// cgroup update doesn't undo the resize cloud-hypervisor already
	// applied, it only means the host-side ceiling didn't move with it.
	m.mu.Lock()
	rv, ok := m.running[vmID]
	m.mu.Unlock()
	if ok {
		if err := cgroup.Apply(vmID, vcpu, memoryMB, rv.pid, rv.numaPin); err != nil {
			slog.Warn("chvmm: cgroup limits not updated after live resize", "vm_id", vmID, "err", err)
		}
	}
	return nil
}

// LiveAddDisk implements vmm.Hotplugger.
func (m *Manager) LiveAddDisk(ctx context.Context, vmID, id, path string) error {
	c, err := m.client(vmID)
	if err != nil {
		return err
	}
	_, err = c.AddDisk(ctx, chapi.DiskConfig{Path: path, ID: id})
	return err
}

// LiveRemoveDevice implements vmm.Hotplugger.
func (m *Manager) LiveRemoveDevice(ctx context.Context, vmID, id string) error {
	c, err := m.client(vmID)
	if err != nil {
		return err
	}
	err = c.RemoveDevice(ctx, id)
	if err != nil && strings.Contains(err.Error(), "not found") {
		// Tolerate an already-removed id -- see vmm.Hotplugger's doc
		// comment: a retried DetachVolume must be safe to call twice.
		return nil
	}
	return err
}
