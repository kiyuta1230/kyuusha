// Package audit emits structured "who did what" records for every request
// api-gateway handles: authentication failures, authorization denials, and
// completed RPCs (success or business-level error). Storage/retention is
// deliberately out of scope here -- these are plain slog JSON log lines,
// meant to be shipped to an external log aggregator (Loki in the
// playground) like any other log. See docs/specs/audit-logging.md.
//
// This package must not import internal/authn or internal/authz: both of
// them call into it (at their own failure/completion points), so it stays
// a leaf to avoid an import cycle.
package audit

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type Event string

const (
	EventAuthnFailed  Event = "authn_failed"
	EventAuthzDenied  Event = "authz_denied"
	EventRPCCompleted Event = "rpc_completed"
)

// Record is one audit event. TenantID/Sub/Role are the caller's claims
// (empty if not yet known -- an authn failure happens before any claims
// exist). RequestTenantID is the tenant_id carried by the request itself,
// which can differ from the caller's own TenantID for admin actions acting
// on another tenant. Err is the outcome; nil means success.
type Record struct {
	Event           Event
	RPCMethod       string
	RequestTenantID string
	TenantID        string
	Sub             string
	Role            string
	Err             error
}

// Log emits r as one structured record, tagged "audit": true so an external
// log pipeline can filter it out from ordinary application logs. If ctx
// carries a valid trace span, its trace_id is attached too, so an audit
// record can be cross-referenced with the trace it happened inside (see
// docs/specs/observability-tracing.md).
func Log(ctx context.Context, r Record) {
	attrs := []any{
		slog.Bool("audit", true),
		slog.String("event", string(r.Event)),
		slog.String("rpc_method", r.RPCMethod),
		slog.String("request_tenant_id", r.RequestTenantID),
		slog.String("tenant_id", r.TenantID),
		slog.String("sub", r.Sub),
		slog.String("role", r.Role),
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
	}
	if r.Err != nil {
		slog.WarnContext(ctx, "audit", append(attrs, slog.String("error", r.Err.Error()))...)
		return
	}
	slog.InfoContext(ctx, "audit", attrs...)
}
