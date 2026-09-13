package image

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var imageCountDesc = prometheus.NewDesc(
	"kyuusha_images_total",
	"Current number of Image objects, by tenant_id and phase.",
	[]string{"tenant_id", "phase"}, nil,
)

// MetricsCollector implements prometheus.Collector, registered onto the
// default registry by cmd/image. Like identity, image has no reconciler
// process (Create either resolves an Image's artifacts synchronously or
// leaves it Pending/Error, no async reconcile loop), so this registers
// directly in the one image binary -- see internal/compute.MetricsCollector's
// doc comment for the fuller reasoning behind that split elsewhere.
type MetricsCollector struct {
	svc *Service
}

// NewMetricsCollector wraps svc for Prometheus collection.
func NewMetricsCollector(svc *Service) *MetricsCollector {
	return &MetricsCollector{svc: svc}
}

func (c *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- imageCountDesc
}

func (c *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	images, err := c.svc.store.List(ctx, "")
	if err != nil {
		return
	}
	counts := make(map[[2]string]int, len(images))
	for _, img := range images {
		counts[[2]string{img.Meta.TenantID, string(img.Status.Phase)}]++
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(imageCountDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
	}
}
