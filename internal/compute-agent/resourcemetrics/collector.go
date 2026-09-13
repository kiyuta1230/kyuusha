// Package resourcemetrics implements the compute-agent side of
// docs/architecture.md「払い出したリソース自身のメトリクス」: a
// prometheus.Collector publishing each currently-running VM's CPU/memory
// usage (from cgroup v2, the same accounting internal/compute-agent/cgroup
// already applies as a resource limit) under tenant_id/vm_id labels, served
// at a separate /metrics/resources endpoint (cmd/compute-agent/main.go) --
// kept apart from /metrics (see internal/telemetry) because these numbers
// are, by design, not folded into any resource_version/Watch-visible state
// (see that doc section's own reasoning).
package resourcemetrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/blockstat"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/netsetup"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/procio"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
)

var (
	cpuUsageDesc = prometheus.NewDesc(
		"kyuusha_vm_cpu_usage_seconds_total",
		"Cumulative CPU time consumed by a VM's VMM process (cgroup v2 cpu.stat usage_usec). Absent for a VM whose cgroup could not be applied (best-effort, see internal/compute-agent/cgroup).",
		[]string{"hypervisor", "tenant_id", "vm_id"}, nil,
	)
	memUsageDesc = prometheus.NewDesc(
		"kyuusha_vm_memory_usage_bytes",
		"Current memory usage of a VM's VMM process (cgroup v2 memory.current).",
		[]string{"hypervisor", "tenant_id", "vm_id"}, nil,
	)
	memLimitDesc = prometheus.NewDesc(
		"kyuusha_vm_memory_limit_bytes",
		`Memory limit applied to a VM's VMM process (cgroup v2 memory.max), equal to spec.memory_mb. Absent when the cgroup reports no limit ("max").`,
		[]string{"hypervisor", "tenant_id", "vm_id"}, nil,
	)
	diskReadBytesDesc = prometheus.NewDesc(
		"kyuusha_vm_disk_read_bytes_total",
		"Cumulative bytes a VM's VMM process has read from any attached disk (root + Volumes combined; /proc/<pid>/io read_bytes) -- unlike the per-Volume metrics below, this works regardless of block-storage backend (NFS included), since it's task-level VFS accounting, not block-layer.",
		[]string{"hypervisor", "tenant_id", "vm_id"}, nil,
	)
	diskWriteBytesDesc = prometheus.NewDesc(
		"kyuusha_vm_disk_write_bytes_total",
		"Cumulative bytes a VM's VMM process has written to any attached disk (root + Volumes combined; /proc/<pid>/io write_bytes). See kyuusha_vm_disk_read_bytes_total.",
		[]string{"hypervisor", "tenant_id", "vm_id"}, nil,
	)
	ifaceRxBytesDesc = prometheus.NewDesc(
		"kyuusha_networkinterface_receive_bytes_total",
		"Cumulative bytes received on a VM's NetworkInterface (its host tap device's rx_bytes).",
		[]string{"hypervisor", "tenant_id", "interface_id"}, nil,
	)
	ifaceTxBytesDesc = prometheus.NewDesc(
		"kyuusha_networkinterface_transmit_bytes_total",
		"Cumulative bytes transmitted on a VM's NetworkInterface (its host tap device's tx_bytes).",
		[]string{"hypervisor", "tenant_id", "interface_id"}, nil,
	)
	volumeReadBytesDesc = prometheus.NewDesc(
		"kyuusha_volume_read_bytes_total",
		"Cumulative bytes read from a VolumeAttachment's backing block device (sysfs stat, sectors*512). Only present for a block-device-backed Volume (ISCSI/NVME_OF) -- an NFS-backed Volume has no per-file host-side I/O counter and is absent here (see kyuusha_vm_disk_read_bytes_total for a VM-level figure that covers NFS too).",
		[]string{"hypervisor", "tenant_id", "attachment_id"}, nil,
	)
	volumeWriteBytesDesc = prometheus.NewDesc(
		"kyuusha_volume_write_bytes_total",
		"Cumulative bytes written to a VolumeAttachment's backing block device. See kyuusha_volume_read_bytes_total.",
		[]string{"hypervisor", "tenant_id", "attachment_id"}, nil,
	)
	volumeReadOpsDesc = prometheus.NewDesc(
		"kyuusha_volume_read_ops_total",
		"Cumulative read operations (IOPS counter) against a VolumeAttachment's backing block device. See kyuusha_volume_read_bytes_total.",
		[]string{"hypervisor", "tenant_id", "attachment_id"}, nil,
	)
	volumeWriteOpsDesc = prometheus.NewDesc(
		"kyuusha_volume_write_ops_total",
		"Cumulative write operations (IOPS counter) against a VolumeAttachment's backing block device. See kyuusha_volume_read_bytes_total.",
		[]string{"hypervisor", "tenant_id", "attachment_id"}, nil,
	)
)

// Collector implements prometheus.Collector. Reads real cgroup v2 stats
// live at scrape time -- both the running-VM set (vmm.VMM.Running) and the
// numbers themselves (cgroup.ReadStats) are cheap to read on demand, so
// there's no background polling loop or cached state to keep consistent
// between scrapes.
type Collector struct {
	Hypervisor string
	// Drivers is the same map[string]vmm.VMM cmd/compute-agent's Agent is
	// built with (one entry per supported driver_hint) -- every driver's
	// Running() is read, since a Hypervisor may have VMs booted under more
	// than one driver_hint at once.
	Drivers map[string]vmm.VMM

	// readStats defaults to cgroup.ReadStats; overridable in tests so
	// Collect can be exercised without a real cgroup v2 filesystem.
	readStats func(vmID string) (cgroup.VMStats, error)
	// readDiskIO defaults to procio.Read; overridable in tests.
	readDiskIO func(pid int) (procio.DiskIO, error)
	// readNetIO defaults to netsetup.Stats; overridable in tests.
	readNetIO func(tapName string) (netsetup.IOStats, error)
	// readBlockIO defaults to blockstat.Stats; overridable in tests.
	readBlockIO func(devicePath string) (blockstat.IO, bool, error)
}

// New builds a Collector reading real host stats.
func New(hypervisor string, drivers map[string]vmm.VMM) *Collector {
	return &Collector{
		Hypervisor: hypervisor, Drivers: drivers,
		readStats: cgroup.ReadStats, readDiskIO: procio.Read,
		readNetIO: netsetup.Stats, readBlockIO: blockstat.Stats,
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cpuUsageDesc
	ch <- memUsageDesc
	ch <- memLimitDesc
	ch <- diskReadBytesDesc
	ch <- diskWriteBytesDesc
	ch <- ifaceRxBytesDesc
	ch <- ifaceTxBytesDesc
	ch <- volumeReadBytesDesc
	ch <- volumeWriteBytesDesc
	ch <- volumeReadOpsDesc
	ch <- volumeWriteOpsDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	readStats := c.readStats
	if readStats == nil {
		readStats = cgroup.ReadStats
	}
	readDiskIO := c.readDiskIO
	if readDiskIO == nil {
		readDiskIO = procio.Read
	}
	readNetIO := c.readNetIO
	if readNetIO == nil {
		readNetIO = netsetup.Stats
	}
	readBlockIO := c.readBlockIO
	if readBlockIO == nil {
		readBlockIO = blockstat.Stats
	}

	for _, driver := range c.Drivers {
		if driver == nil {
			continue
		}
		for _, rv := range driver.Running() {
			if stats, err := readStats(rv.VMID); err == nil {
				ch <- prometheus.MustNewConstMetric(cpuUsageDesc, prometheus.CounterValue, stats.CPUUsageSeconds, c.Hypervisor, rv.TenantID, rv.VMID)
				ch <- prometheus.MustNewConstMetric(memUsageDesc, prometheus.GaugeValue, float64(stats.MemoryUsageBytes), c.Hypervisor, rv.TenantID, rv.VMID)
				if stats.MemoryLimitBytes >= 0 {
					ch <- prometheus.MustNewConstMetric(memLimitDesc, prometheus.GaugeValue, float64(stats.MemoryLimitBytes), c.Hypervisor, rv.TenantID, rv.VMID)
				}
			}
			// Best-effort throughout below, same spirit as cgroup.Apply
			// itself: a VM/interface/volume this host can't read stats for
			// simply has no metrics here, not an error worth logging on
			// every single scrape.

			if rv.PID > 0 {
				if io, err := readDiskIO(rv.PID); err == nil {
					ch <- prometheus.MustNewConstMetric(diskReadBytesDesc, prometheus.CounterValue, float64(io.ReadBytes), c.Hypervisor, rv.TenantID, rv.VMID)
					ch <- prometheus.MustNewConstMetric(diskWriteBytesDesc, prometheus.CounterValue, float64(io.WriteBytes), c.Hypervisor, rv.TenantID, rv.VMID)
				}
			}

			for _, ifaceID := range rv.NetworkInterfaces {
				io, err := readNetIO(netsetup.TapName(ifaceID))
				if err != nil {
					continue
				}
				ch <- prometheus.MustNewConstMetric(ifaceRxBytesDesc, prometheus.CounterValue, float64(io.RxBytes), c.Hypervisor, rv.TenantID, ifaceID)
				ch <- prometheus.MustNewConstMetric(ifaceTxBytesDesc, prometheus.CounterValue, float64(io.TxBytes), c.Hypervisor, rv.TenantID, ifaceID)
			}

			for _, vol := range rv.Volumes {
				io, ok, err := readBlockIO(vol.DevicePath)
				if err != nil || !ok {
					continue
				}
				ch <- prometheus.MustNewConstMetric(volumeReadBytesDesc, prometheus.CounterValue, float64(io.ReadBytes), c.Hypervisor, rv.TenantID, vol.AttachmentID)
				ch <- prometheus.MustNewConstMetric(volumeWriteBytesDesc, prometheus.CounterValue, float64(io.WriteBytes), c.Hypervisor, rv.TenantID, vol.AttachmentID)
				ch <- prometheus.MustNewConstMetric(volumeReadOpsDesc, prometheus.CounterValue, float64(io.ReadOps), c.Hypervisor, rv.TenantID, vol.AttachmentID)
				ch <- prometheus.MustNewConstMetric(volumeWriteOpsDesc, prometheus.CounterValue, float64(io.WriteOps), c.Hypervisor, rv.TenantID, vol.AttachmentID)
			}
		}
	}
}
