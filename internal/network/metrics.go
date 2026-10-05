package network

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	subnetCountDesc = prometheus.NewDesc(
		"kyuusha_subnets_total",
		"Current number of Subnet objects, by tenant_id and phase.",
		[]string{"tenant_id", "phase"}, nil,
	)
	networkInterfaceCountDesc = prometheus.NewDesc(
		"kyuusha_networkinterfaces_total",
		"Current number of NetworkInterface objects, by tenant_id and phase.",
		[]string{"tenant_id", "phase"}, nil,
	)
)

// MetricsCollector implements prometheus.Collector -- see
// internal/compute.MetricsCollector's doc comment for the full reasoning
// (same pattern, registered by cmd/network-reconciler). network has no
// per-tenant quota concept of its own (see docs/architecture.md「Quota設計」:
// quota lives only in compute/block-storage), so this only ever emits
// resource counts, not a kyuusha_tenant_quota_used series.
type MetricsCollector struct {
	svc *Service
}

// NewMetricsCollector wraps svc for Prometheus collection.
func NewMetricsCollector(svc *Service) *MetricsCollector {
	return &MetricsCollector{svc: svc}
}

func (c *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- subnetCountDesc
	ch <- networkInterfaceCountDesc
	setUpdatesSent.Describe(ch)
}

func (c *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	setUpdatesSent.Collect(ch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if subnets, err := c.svc.subnets.List(ctx, ""); err == nil {
		counts := make(map[[2]string]int, len(subnets))
		for _, sn := range subnets {
			counts[[2]string{sn.Meta.TenantID, string(sn.Status.Phase)}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(subnetCountDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
		}
	}

	if ifaces, err := c.svc.interfaces.List(ctx, ""); err == nil {
		counts := make(map[[2]string]int, len(ifaces))
		for _, n := range ifaces {
			counts[[2]string{n.Meta.TenantID, string(n.Status.Phase)}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(networkInterfaceCountDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
		}
	}
}
