package resourcemetrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/cgroup"
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
