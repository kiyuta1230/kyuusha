package telemetry

import (
	"context"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// InjectNATSHeader writes ctx's current span context into header as W3C
// traceparent, for a message about to be published.
func InjectNATSHeader(ctx context.Context, header nats.Header) {
	otel.GetTextMapPropagator().Inject(ctx, NATSHeaderCarrier{Header: header})
}

// LinkFromNATSHeader extracts a span context from a consumed message's
// header (if present) as a trace.Link, for starting a new span that's
// related to -- but not a strict parent of -- whatever produced the
// message. See docs/specs/nats-messaging.md: a NATS hop's producer and
// consumer are separated by an unpredictable delay, so linking (not
// parent-child) avoids a span whose duration is however long the message
// sat queued. A message with no traceparent header yields an empty Link,
// which the SDK drops.
func LinkFromNATSHeader(header nats.Header) trace.Link {
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), NATSHeaderCarrier{Header: header})
	return trace.Link{SpanContext: trace.SpanContextFromContext(ctx)}
}
