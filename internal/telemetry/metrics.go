package telemetry

import (
	"context"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// SetupMetrics configures a global MeterProvider backed by a Prometheus
// exporter and returns the http.Handler to serve at /metrics. otelgrpc's
// stats handler (already wired for tracing -- see Setup) reports gRPC
// server/client request counts and durations through this same
// MeterProvider automatically, with no separate instrumentation needed;
// Go runtime metrics (goroutines, GC, memory) are added the same way.
//
// Unlike Setup, this always runs (there's no "disabled" mode): metrics are
// pull-based and cheap to expose even if nothing ever scrapes them.
func SetupMetrics(serviceName string) (http.Handler, func(context.Context) error, error) {
	exporter, err := otelprometheus.New()
	if err != nil {
		return nil, nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	res, err := resource.New(context.Background(), resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, nil, fmt.Errorf("create otel resource: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter), sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, nil, fmt.Errorf("start go runtime metrics: %w", err)
	}

	return promhttp.Handler(), mp.Shutdown, nil
}
