package blockstorage

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	volumeCountDesc = prometheus.NewDesc(
		"kyuusha_volumes_total",
		"Current number of Volume objects, by tenant_id and phase.",
		[]string{"tenant_id", "phase"}, nil,
	)
	volumeAttachmentCountDesc = prometheus.NewDesc(
		"kyuusha_volumeattachments_total",
		"Current number of VolumeAttachment objects, by tenant_id and phase.",
		[]string{"tenant_id", "phase"}, nil,
	)
	quotaUsedDesc = prometheus.NewDesc(
		"kyuusha_tenant_quota_used",
		"block-storage's own tenant_usage accounting (see docs/architecture.md「Quota設計」), by tenant_id and resource (volume_gb). The matching limit is exposed by identity as kyuusha_tenant_quota_limit.",
		[]string{"tenant_id", "resource"}, nil,
	)
)

// MetricsCollector implements prometheus.Collector -- see
// internal/compute.MetricsCollector's doc comment for the full reasoning
// (same pattern, registered by cmd/block-storage-reconciler).
type MetricsCollector struct {
	svc *Service
}

// NewMetricsCollector wraps svc for Prometheus collection.
func NewMetricsCollector(svc *Service) *MetricsCollector {
	return &MetricsCollector{svc: svc}
}

func (c *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- volumeCountDesc
	ch <- volumeAttachmentCountDesc
	ch <- quotaUsedDesc
}

func (c *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if volumes, err := c.svc.volumes.List(ctx, ""); err == nil {
		counts := make(map[[2]string]int, len(volumes))
		for _, v := range volumes {
			counts[[2]string{v.Meta.TenantID, string(v.Status.Phase)}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(volumeCountDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
		}
	}

	if attachments, err := c.svc.attachments.List(ctx, ""); err == nil {
		counts := make(map[[2]string]int, len(attachments))
		for _, a := range attachments {
			counts[[2]string{a.Meta.TenantID, string(a.Status.Phase)}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(volumeAttachmentCountDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
		}
	}

	c.svc.usageMu.Lock()
	usage := make(map[string]tenantUsage, len(c.svc.usage))
	for tenantID, u := range c.svc.usage {
		usage[tenantID] = u
	}
	c.svc.usageMu.Unlock()
	for tenantID, u := range usage {
		ch <- prometheus.MustNewConstMetric(quotaUsedDesc, prometheus.GaugeValue, float64(u.VolumeGB), tenantID, "volume_gb")
	}
}
