package identity

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	quotaLimitDesc = prometheus.NewDesc(
		"kyuusha_tenant_quota_limit",
		"Each Tenant's own Quota limit, by tenant_id and resource (vcpu|memory_mb|volume_gb|vms). The matching usage is exposed by compute (vcpu|memory_mb|vms) and block-storage (volume_gb) as kyuusha_tenant_quota_used.",
		[]string{"tenant_id", "resource"}, nil,
	)
	tenantCountDesc = prometheus.NewDesc(
		"kyuusha_tenants_total",
		"Current number of Tenant objects, by phase.",
		[]string{"phase"}, nil,
	)
)

// MetricsCollector implements prometheus.Collector, registered onto the
// default registry by cmd/identity. identity is the one service in this
// split that owns Quota limits directly (Tenant.Spec.Quota) with no
// cross-service RPC needed, unlike usage (see internal/compute and
// internal/block-storage's own MetricsCollectors) which lives wherever
// the resource being counted against that quota actually lives -- identity
// has no reconciler process (it's plain CRUD+Watch, no async reconcile
// loop), so this registers directly in the one identity binary.
type MetricsCollector struct {
	svc *Service
}

// NewMetricsCollector wraps svc for Prometheus collection.
func NewMetricsCollector(svc *Service) *MetricsCollector {
	return &MetricsCollector{svc: svc}
}

func (c *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- quotaLimitDesc
	ch <- tenantCountDesc
}

func (c *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tenants, err := c.svc.store.List(ctx, "")
	if err != nil {
		return
	}
	counts := make(map[string]int, len(tenants))
	for _, t := range tenants {
		q := t.Spec.Quota
		ch <- prometheus.MustNewConstMetric(quotaLimitDesc, prometheus.GaugeValue, float64(q.MaxVCPU), t.Meta.ID, "vcpu")
		ch <- prometheus.MustNewConstMetric(quotaLimitDesc, prometheus.GaugeValue, float64(q.MaxMemoryMB), t.Meta.ID, "memory_mb")
		ch <- prometheus.MustNewConstMetric(quotaLimitDesc, prometheus.GaugeValue, float64(q.MaxVolumeGB), t.Meta.ID, "volume_gb")
		ch <- prometheus.MustNewConstMetric(quotaLimitDesc, prometheus.GaugeValue, float64(q.MaxVMs), t.Meta.ID, "vms")
		counts[string(t.Status.Phase)]++
	}
	for phase, n := range counts {
		ch <- prometheus.MustNewConstMetric(tenantCountDesc, prometheus.GaugeValue, float64(n), phase)
	}
}
