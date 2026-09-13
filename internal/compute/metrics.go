package compute

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	vmCountDesc = prometheus.NewDesc(
		"kyuusha_virtualmachines_total",
		"Current number of VirtualMachine objects, by tenant_id and phase.",
		[]string{"tenant_id", "phase"}, nil,
	)
	quotaUsedDesc = prometheus.NewDesc(
		"kyuusha_tenant_quota_used",
		"compute's own tenant_usage accounting (see docs/architecture.md「Quota設計」), by tenant_id and resource (vcpu|memory_mb|vms). The matching limit is exposed by identity as kyuusha_tenant_quota_limit.",
		[]string{"tenant_id", "resource"}, nil,
	)
)

// MetricsCollector implements prometheus.Collector, registered onto the
// default registry (the same one telemetry.SetupMetrics' otelprometheus
// exporter uses) by cmd/compute-reconciler -- see that binary's own
// reasoning for why this lives in the reconciler, not the API binary:
// unlike compute-agent's per-VM /metrics/resources (a separate endpoint,
// deliberately kept off resource_version/Watch), resource counts and
// quota usage are ordinary control-plane/business metrics, exactly the
// same kind of thing /metrics already carries -- they just weren't
// implemented yet. Running this on N stateless API replicas would mean N
// redundant full etcd List() scans every scrape; the reconciler is
// already the one always-singleton process per service.
type MetricsCollector struct {
	svc *Service
}

// NewMetricsCollector wraps svc for Prometheus collection.
func NewMetricsCollector(svc *Service) *MetricsCollector {
	return &MetricsCollector{svc: svc}
}

func (c *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- vmCountDesc
	ch <- quotaUsedDesc
}

func (c *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Live List+tally at scrape time, same "no background polling, no
	// cached state" choice as internal/compute-agent/resourcemetrics --
	// there's no correctness reason to keep a copy around between scrapes,
	// and internal/resource.Store.List always fully decodes every object
	// anyway (no cheaper count-only path), a cost the reconciler's own
	// periodic sweeps already pay routinely.
	if vms, err := c.svc.store.List(ctx, ""); err == nil {
		counts := make(map[[2]string]int, len(vms))
		for _, vm := range vms {
			counts[[2]string{vm.Meta.TenantID, string(vm.Status.Phase)}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(vmCountDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
		}
	}

	c.svc.usageMu.Lock()
	usage := make(map[string]tenantUsage, len(c.svc.usage))
	for tenantID, u := range c.svc.usage {
		usage[tenantID] = u
	}
	c.svc.usageMu.Unlock()
	for tenantID, u := range usage {
		ch <- prometheus.MustNewConstMetric(quotaUsedDesc, prometheus.GaugeValue, float64(u.VCPU), tenantID, "vcpu")
		ch <- prometheus.MustNewConstMetric(quotaUsedDesc, prometheus.GaugeValue, float64(u.MemoryMB), tenantID, "memory_mb")
		ch <- prometheus.MustNewConstMetric(quotaUsedDesc, prometheus.GaugeValue, float64(u.VMCount), tenantID, "vms")
	}
}
