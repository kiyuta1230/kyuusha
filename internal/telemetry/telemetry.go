// Package telemetry wires a process into distributed tracing: an OTLP/gRPC
// exporter, a global TracerProvider, and the W3C traceparent propagator used
// both by otelgrpc (automatic, for every gRPC call) and manually for NATS
// message headers (see docs/specs/nats-messaging.md's tracing design).
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Setup configures global tracing for serviceName. If otlpEndpoint is empty,
// tracing is left disabled (otel's default no-op TracerProvider stays in
// effect) -- so `go run`/tests don't need a collector running. Otherwise it
// dials otlpEndpoint (plaintext; same follow-up as every other inter-service
// connection in this system) and exports every span, unsampled: this system
// is far below any scale where sampling is worth the loss of visibility.
//
// The returned shutdown func flushes pending spans; callers should defer it
// (with a short timeout context) before process exit.
func Setup(ctx context.Context, serviceName, otlpEndpoint string) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if otlpEndpoint == "" {
		return noop, nil
	}

	conn, err := grpc.NewClient(otlpEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return noop, fmt.Errorf("dial otlp endpoint %s: %w", otlpEndpoint, err)
	}

	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return noop, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return noop, fmt.Errorf("create otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}
