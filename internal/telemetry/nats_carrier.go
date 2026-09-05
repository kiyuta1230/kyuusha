package telemetry

import "github.com/nats-io/nats.go"

// NATSHeaderCarrier adapts nats.Header to propagation.TextMapCarrier, so
// otel.GetTextMapPropagator().Inject/Extract can read and write W3C
// traceparent through a NATS message's headers -- there is no gRPC metadata
// equivalent on this side of the CMD/EVT boundary, so this is done by hand.
type NATSHeaderCarrier struct{ Header nats.Header }

func (c NATSHeaderCarrier) Get(key string) string {
	return c.Header.Get(key)
}

func (c NATSHeaderCarrier) Set(key, value string) {
	c.Header.Set(key, value)
}

func (c NATSHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c.Header))
	for k := range c.Header {
		keys = append(keys, k)
	}
	return keys
}
