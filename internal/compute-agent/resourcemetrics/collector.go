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

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
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
}

// New builds a Collector reading real cgroup stats.
func New(hypervisor string, drivers map[string]vmm.VMM) *Collector {
	return &Collector{Hypervisor: hypervisor, Drivers: drivers, readStats: cgroup.ReadStats}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cpuUsageDesc
	ch <- memUsageDesc
	ch <- memLimitDesc
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	readStats := c.readStats
	if readStats == nil {
		readStats = cgroup.ReadStats
	}
	for _, driver := range c.Drivers {
		if driver == nil {
			continue
		}
		for _, rv := range driver.Running() {
			stats, err := readStats(rv.VMID)
			if err != nil {
				// Best-effort, same spirit as cgroup.Apply itself: a VM
				// whose cgroup limits could not be applied (e.g. no
				// delegation on this host) simply has no metrics here, not
				// an error worth logging on every single scrape.
				continue
			}
			ch <- prometheus.MustNewConstMetric(cpuUsageDesc, prometheus.CounterValue, stats.CPUUsageSeconds, c.Hypervisor, rv.TenantID, rv.VMID)
			ch <- prometheus.MustNewConstMetric(memUsageDesc, prometheus.GaugeValue, float64(stats.MemoryUsageBytes), c.Hypervisor, rv.TenantID, rv.VMID)
			if stats.MemoryLimitBytes >= 0 {
				ch <- prometheus.MustNewConstMetric(memLimitDesc, prometheus.GaugeValue, float64(stats.MemoryLimitBytes), c.Hypervisor, rv.TenantID, rv.VMID)
			}
		}
	}
}
