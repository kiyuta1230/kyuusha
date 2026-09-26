package resourcemetrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/blockstat"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/netsetup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/procio"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
)

// fakeVMM is a minimal vmm.VMM stub: Collect only ever calls Running, so
// every other method just needs to exist to satisfy the interface.
type fakeVMM struct {
	running []vmm.RunningVM
}

func (f fakeVMM) Boot(context.Context, vmm.BootSpec) ([]vmm.AttachedVolume, error) { return nil, nil }
func (f fakeVMM) Stop(string, bool)                                                {}
func (f fakeVMM) Destroy(string)                                                   {}
func (f fakeVMM) ConsoleLogPath(string) string                                     { return "" }
func (f fakeVMM) Running() []vmm.RunningVM                                         { return f.running }
func (f fakeVMM) RootDiskPath(string) (string, error)                              { return "", nil }
func (f fakeVMM) ApplyACL(string, string, string, string, []vmm.FirewallRule, []vmm.FirewallRule) (bool, error) {
	return false, nil
}

func TestCollector_EmitsMetricsForRunningVMs(t *testing.T) {
	c := &Collector{
		Hypervisor: "hypervisor-1",
		Drivers: map[string]vmm.VMM{
			"FIRECRACKER": fakeVMM{running: []vmm.RunningVM{
				{VMID: "vm-1", TenantID: "tenant-a"},
			}},
		},
		readStats: func(vmID string) (cgroup.VMStats, error) {
			if vmID != "vm-1" {
				t.Fatalf("unexpected vmID %q", vmID)
			}
			return cgroup.VMStats{CPUUsageSeconds: 12.5, MemoryUsageBytes: 1024, MemoryLimitBytes: 4096}, nil
		},
	}

	want := `
# HELP kyuusha_vm_cpu_usage_seconds_total Cumulative CPU time consumed by a VM's VMM process (cgroup v2 cpu.stat usage_usec). Absent for a VM whose cgroup could not be applied (best-effort, see internal/compute-agent/cgroup).
# TYPE kyuusha_vm_cpu_usage_seconds_total counter
kyuusha_vm_cpu_usage_seconds_total{hypervisor="hypervisor-1",tenant_id="tenant-a",vm_id="vm-1"} 12.5
# HELP kyuusha_vm_memory_limit_bytes Memory limit applied to a VM's VMM process (cgroup v2 memory.max), equal to spec.memory_mb. Absent when the cgroup reports no limit ("max").
# TYPE kyuusha_vm_memory_limit_bytes gauge
kyuusha_vm_memory_limit_bytes{hypervisor="hypervisor-1",tenant_id="tenant-a",vm_id="vm-1"} 4096
# HELP kyuusha_vm_memory_usage_bytes Current memory usage of a VM's VMM process (cgroup v2 memory.current).
# TYPE kyuusha_vm_memory_usage_bytes gauge
kyuusha_vm_memory_usage_bytes{hypervisor="hypervisor-1",tenant_id="tenant-a",vm_id="vm-1"} 1024
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}

// TestCollector_SkipsVMsWithoutCgroupStats covers a VM whose cgroup limits
// were never applied (no delegation on this host, see cgroup.Apply's own
// best-effort doc comment) -- it must simply be absent from the output, not
// cause an error or a partial/zeroed metric.
func TestCollector_SkipsVMsWithoutCgroupStats(t *testing.T) {
	c := &Collector{
		Hypervisor: "hypervisor-1",
		Drivers: map[string]vmm.VMM{
			"FIRECRACKER": fakeVMM{running: []vmm.RunningVM{
				{VMID: "vm-no-cgroup", TenantID: "tenant-a"},
			}},
		},
		readStats: func(vmID string) (cgroup.VMStats, error) {
			return cgroup.VMStats{}, errors.New("cgroup not available")
		},
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader("")); err != nil {
		t.Fatalf("expected no metrics, got: %v", err)
	}
}

// TestCollector_OmitsMemoryLimitWhenUnbounded covers ReadStats reporting
// MemoryLimitBytes as -1 (cgroup's memory.max == "max", i.e. no limit) --
// the limit metric must be omitted entirely, not emitted as -1.
func TestCollector_OmitsMemoryLimitWhenUnbounded(t *testing.T) {
	c := &Collector{
		Hypervisor: "hypervisor-1",
		Drivers: map[string]vmm.VMM{
			"FIRECRACKER": fakeVMM{running: []vmm.RunningVM{
				{VMID: "vm-1", TenantID: "tenant-a"},
			}},
		},
		readStats: func(vmID string) (cgroup.VMStats, error) {
			return cgroup.VMStats{CPUUsageSeconds: 1, MemoryUsageBytes: 2, MemoryLimitBytes: -1}, nil
		},
	}

	want := `
# HELP kyuusha_vm_cpu_usage_seconds_total Cumulative CPU time consumed by a VM's VMM process (cgroup v2 cpu.stat usage_usec). Absent for a VM whose cgroup could not be applied (best-effort, see internal/compute-agent/cgroup).
# TYPE kyuusha_vm_cpu_usage_seconds_total counter
kyuusha_vm_cpu_usage_seconds_total{hypervisor="hypervisor-1",tenant_id="tenant-a",vm_id="vm-1"} 1
# HELP kyuusha_vm_memory_usage_bytes Current memory usage of a VM's VMM process (cgroup v2 memory.current).
# TYPE kyuusha_vm_memory_usage_bytes gauge
kyuusha_vm_memory_usage_bytes{hypervisor="hypervisor-1",tenant_id="tenant-a",vm_id="vm-1"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "kyuusha_vm_cpu_usage_seconds_total", "kyuusha_vm_memory_usage_bytes", "kyuusha_vm_memory_limit_bytes"); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}

func noStats(string) (cgroup.VMStats, error) {
	return cgroup.VMStats{}, errors.New("not used in this test")
}

// TestCollector_EmitsVMDiskIO covers kyuusha_vm_disk_{read,write}_bytes_total
// -- the /proc/<pid>/io-based figure that works for a VM regardless of its
// Volumes' backend (see procio's doc comment).
func TestCollector_EmitsVMDiskIO(t *testing.T) {
	c := &Collector{
		Hypervisor: "hypervisor-1",
		Drivers: map[string]vmm.VMM{
			"FIRECRACKER": fakeVMM{running: []vmm.RunningVM{
				{VMID: "vm-1", TenantID: "tenant-a", PID: 4242},
			}},
		},
		readStats: noStats,
		readDiskIO: func(pid int) (procio.DiskIO, error) {
			if pid != 4242 {
				t.Fatalf("unexpected pid %d", pid)
			}
			return procio.DiskIO{ReadBytes: 1000, WriteBytes: 2000}, nil
		},
	}

	want := `
# HELP kyuusha_vm_disk_read_bytes_total Cumulative bytes a VM's VMM process has read from any attached disk (root + Volumes combined; /proc/<pid>/io read_bytes) -- unlike the per-Volume metrics below, this works regardless of block-storage backend (NFS included), since it's task-level VFS accounting, not block-layer.
# TYPE kyuusha_vm_disk_read_bytes_total counter
kyuusha_vm_disk_read_bytes_total{hypervisor="hypervisor-1",tenant_id="tenant-a",vm_id="vm-1"} 1000
# HELP kyuusha_vm_disk_write_bytes_total Cumulative bytes a VM's VMM process has written to any attached disk (root + Volumes combined; /proc/<pid>/io write_bytes). See kyuusha_vm_disk_read_bytes_total.
# TYPE kyuusha_vm_disk_write_bytes_total counter
kyuusha_vm_disk_write_bytes_total{hypervisor="hypervisor-1",tenant_id="tenant-a",vm_id="vm-1"} 2000
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "kyuusha_vm_disk_read_bytes_total", "kyuusha_vm_disk_write_bytes_total"); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}

// TestCollector_SkipsVMDiskIOWhenUnavailable covers a VM whose PID is 0
// (Running reported it without a real process -- shouldn't happen in
// practice, but Collect must not call readDiskIO with a meaningless pid)
// and one whose readDiskIO call fails outright.
func TestCollector_SkipsVMDiskIOWhenUnavailable(t *testing.T) {
	c := &Collector{
		Hypervisor: "hypervisor-1",
		Drivers: map[string]vmm.VMM{
			"FIRECRACKER": fakeVMM{running: []vmm.RunningVM{
				{VMID: "vm-no-pid", TenantID: "tenant-a"},
				{VMID: "vm-read-fails", TenantID: "tenant-a", PID: 99},
			}},
		},
		readStats: noStats,
		readDiskIO: func(pid int) (procio.DiskIO, error) {
			return procio.DiskIO{}, errors.New("/proc/99/io: no such process")
		},
	}

	if err := testutil.CollectAndCompare(c, strings.NewReader(""), "kyuusha_vm_disk_read_bytes_total", "kyuusha_vm_disk_write_bytes_total"); err != nil {
		t.Fatalf("expected no metrics, got: %v", err)
	}
}

// TestCollector_EmitsNetworkInterfaceIO covers
// kyuusha_networkinterface_{receive,transmit}_bytes_total -- the collector
// must derive the tap name from the NetworkInterface id itself
// (netsetup.TapName), not have it handed in separately.
func TestCollector_EmitsNetworkInterfaceIO(t *testing.T) {
	ifaceID := "netif-abc123"
	wantTap := netsetup.TapName(ifaceID)

	c := &Collector{
		Hypervisor: "hypervisor-1",
		Drivers: map[string]vmm.VMM{
			"FIRECRACKER": fakeVMM{running: []vmm.RunningVM{
				{VMID: "vm-1", TenantID: "tenant-a", NetworkInterfaces: []string{ifaceID}},
			}},
		},
		readStats: noStats,
		readNetIO: func(tap string) (netsetup.IOStats, error) {
			if tap != wantTap {
				t.Fatalf("unexpected tap name %q, want %q", tap, wantTap)
			}
			return netsetup.IOStats{RxBytes: 111, TxBytes: 222}, nil
		},
	}

	want := `
# HELP kyuusha_networkinterface_receive_bytes_total Cumulative bytes received on a VM's NetworkInterface (its host tap device's rx_bytes).
# TYPE kyuusha_networkinterface_receive_bytes_total counter
kyuusha_networkinterface_receive_bytes_total{hypervisor="hypervisor-1",interface_id="netif-abc123",tenant_id="tenant-a"} 111
# HELP kyuusha_networkinterface_transmit_bytes_total Cumulative bytes transmitted on a VM's NetworkInterface (its host tap device's tx_bytes).
# TYPE kyuusha_networkinterface_transmit_bytes_total counter
kyuusha_networkinterface_transmit_bytes_total{hypervisor="hypervisor-1",interface_id="netif-abc123",tenant_id="tenant-a"} 222
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "kyuusha_networkinterface_receive_bytes_total", "kyuusha_networkinterface_transmit_bytes_total"); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}

// TestCollector_EmitsVolumeIOForBlockDevicesOnly covers
// kyuusha_volume_{read,write}_{bytes,ops}_total: present for a
// block-device-backed attachment, absent (not zeroed) for an NFS-backed one
// (readBlockIO's ok=false) -- see blockstat's doc comment.
func TestCollector_EmitsVolumeIOForBlockDevicesOnly(t *testing.T) {
	c := &Collector{
		Hypervisor: "hypervisor-1",
		Drivers: map[string]vmm.VMM{
			"FIRECRACKER": fakeVMM{running: []vmm.RunningVM{
				{VMID: "vm-1", TenantID: "tenant-a", Volumes: []vmm.AttachedVolume{
					{AttachmentID: "volattach-block", TenantID: "tenant-a", DevicePath: "/dev/disk/by-id/block-one"},
					{AttachmentID: "volattach-nfs", TenantID: "tenant-a", DevicePath: "/mnt/nfs/vol.img"},
				}},
			}},
		},
		readStats: noStats,
		readBlockIO: func(path string) (blockstat.IO, bool, error) {
			if path == "/mnt/nfs/vol.img" {
				return blockstat.IO{}, false, nil
			}
			return blockstat.IO{ReadBytes: 5000, WriteBytes: 6000, ReadOps: 50, WriteOps: 60}, true, nil
		},
	}

	want := `
# HELP kyuusha_volume_read_bytes_total Cumulative bytes read from a VolumeAttachment's backing block device (sysfs stat, sectors*512). Only present for a block-device-backed Volume (ISCSI/NVME_OF) -- an NFS-backed Volume has no per-file host-side I/O counter and is absent here (see kyuusha_vm_disk_read_bytes_total for a VM-level figure that covers NFS too).
# TYPE kyuusha_volume_read_bytes_total counter
kyuusha_volume_read_bytes_total{attachment_id="volattach-block",hypervisor="hypervisor-1",tenant_id="tenant-a"} 5000
# HELP kyuusha_volume_write_bytes_total Cumulative bytes written to a VolumeAttachment's backing block device. See kyuusha_volume_read_bytes_total.
# TYPE kyuusha_volume_write_bytes_total counter
kyuusha_volume_write_bytes_total{attachment_id="volattach-block",hypervisor="hypervisor-1",tenant_id="tenant-a"} 6000
# HELP kyuusha_volume_read_ops_total Cumulative read operations (IOPS counter) against a VolumeAttachment's backing block device. See kyuusha_volume_read_bytes_total.
# TYPE kyuusha_volume_read_ops_total counter
kyuusha_volume_read_ops_total{attachment_id="volattach-block",hypervisor="hypervisor-1",tenant_id="tenant-a"} 50
# HELP kyuusha_volume_write_ops_total Cumulative write operations (IOPS counter) against a VolumeAttachment's backing block device. See kyuusha_volume_read_bytes_total.
# TYPE kyuusha_volume_write_ops_total counter
kyuusha_volume_write_ops_total{attachment_id="volattach-block",hypervisor="hypervisor-1",tenant_id="tenant-a"} 60
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want),
		"kyuusha_volume_read_bytes_total", "kyuusha_volume_write_bytes_total",
		"kyuusha_volume_read_ops_total", "kyuusha_volume_write_ops_total"); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}
