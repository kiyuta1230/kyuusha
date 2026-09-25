// Package admissionwebhook is a small, resource-agnostic client for
// kyuusha's Create-side external validation gate -- the
// "ValidatingAdmissionWebhook相当" docs/specs/external-integration.md
// 「ゲート系(作成側)」names as the one piece of Finalizer's motivating pair
// (docs/architecture.md「Finalizer」) left unimplemented. Unlike Finalizer
// (asynchronous, Watch-driven), this
// is a synchronous HTTP call made from inside a Create RPC's own request
// path, so it directly couples that RPC's availability to every configured
// webhook's -- see Gate's FailOpen field for the resulting tradeoff this
// package makes configurable rather than picking one side for every
// deployment.
//
// Deliberately not gRPC: an external validator is exactly the kind of
// third-party tooling kyuusha doesn't control the implementation language
// or stack of, so this uses plain HTTP POST + JSON (mirroring Kubernetes'
// own AdmissionReview convention) rather than requiring a generated gRPC
// client. Deliberately validating-only, not mutating (see Response's doc
// comment) -- docs/specs/external-integration.md names this
// "ValidatingAdmissionWebhook相当" specifically, not the mutating variant,
// which would need merge-patch semantics this package doesn't implement.
//
// Security: which URLs a Gate calls is entirely an operator-time
// configuration decision (a service binary's own startup flags -- see
// cmd/compute/main.go's -admission-webhook-urls), never something exposed
// through kyuusha's own API surface for a tenant to register. This sidesteps
// the "誰がwebhookを登録できるか" concern docs/architecture.md's Finalizer
// section raised as one reason this was deferred -- same reasoning as
// internal/compute-agent/netsetup's VNAP plugin binary being operator-
// configured only, never tenant-facing.
package admissionwebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Request is what a Gate sends to every configured webhook URL, as a JSON
// POST body. Spec is deliberately opaque (json.RawMessage) here: building
// the actual wire-shaped JSON for a given resource type's spec is that
// resource type's own Service's job (see internal/compute/service.go's
// Create for VirtualMachine's), not something this resource-agnostic
// package should know the shape of.
type Request struct {
	Operation string          `json:"operation"` // "CREATE" -- the only value this package sends today
	Resource  string          `json:"resource"`  // e.g. "VirtualMachine"
	TenantID  string          `json:"tenant_id"`
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
}

// Response is what a webhook is expected to answer with. Reason is
// surfaced to the caller (and from there, back to kyuusha's own client) as
// the rejection message when Allowed is false; ignored otherwise.
type Response struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// Gate holds a set of admission webhook URLs a Create call must clear.
// The zero value (nil URLs) is a safe, always-allow no-op -- callers don't
// need to guard calling Validate behind an "is this enabled" check.
type Gate struct {
	// URLs are called concurrently on every Validate call. All must
	// explicitly allow (Response.Allowed == true) for Validate to allow --
	// same "every validating webhook must agree" semantics Kubernetes
	// itself uses, not "any one approval is enough".
	URLs []string
	// Timeout bounds each webhook call. Zero means DefaultTimeout.
	Timeout time.Duration
	// FailOpen controls what an unreachable/erroring webhook (not an
	// explicit Response{Allowed: false} -- that's always a hard deny,
	// regardless of this field) means: false (the default, and the
	// deliberately safe choice -- see docs/architecture.md's Finalizer
	// section on the availability-coupling concern this raises) treats it
	// as a deny; true treats it as an implicit allow, logging a warning
	// instead. There is no per-URL override in this first version -- one
	// Gate's URLs all share the same policy.
	FailOpen bool
	// Client is the http.Client used for every call; nil means
	// http.DefaultClient. Overridable for tests.
	Client *http.Client
}

// DefaultTimeout is used when Gate.Timeout is zero. Deliberately short:
// this blocks a client's Create RPC for as long as the slowest configured
// webhook takes, so a webhook that needs longer than this to decide is a
// design smell in the webhook, not something to accommodate by raising
// this default.
const DefaultTimeout = 3 * time.Second

// Validate calls every one of g.URLs concurrently with req as the POST
// body, and reports:
//
//   - (true, "", nil): every webhook allowed (or g.URLs is empty)
//   - (false, reason, nil): at least one webhook explicitly denied
//     (Response.Allowed == false) -- a policy decision, not an
//     infrastructure failure. reason is that webhook's own Response.Reason.
//   - (false, "", err): fail-closed (g.FailOpen == false) triggered by a
//     webhook that was unreachable, timed out, or returned a malformed
//     response -- an infrastructure failure, not a policy decision. err
//     wraps the underlying cause.
//
// An explicit deny always wins over a fail-open pass for a *different*
// webhook: if one webhook denies and another is simultaneously
// unreachable, the deny is what gets reported (denying is never something
// FailOpen overrides).
func (g *Gate) Validate(ctx context.Context, req Request) (allowed bool, reason string, err error) {
	if len(g.URLs) == 0 {
		return true, "", nil
	}
	payload, marshalErr := json.Marshal(req)
	if marshalErr != nil {
		return false, "", fmt.Errorf("admissionwebhook: marshal request: %w", marshalErr)
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel() // also aborts any still-in-flight calls once Validate returns early

	type result struct {
		url     string
		allowed bool
		reason  string
		err     error
	}
	results := make(chan result, len(g.URLs))
	for _, url := range g.URLs {
		go func(url string) {
			a, r, e := g.call(ctx, url, payload)
			results <- result{url, a, r, e}
		}(url)
	}

	var denyReason string
	var denied bool
	var unavailableErr error
	for range g.URLs {
		r := <-results
		switch {
		case r.err != nil:
			slog.Warn("admissionwebhook: call failed", "url", r.url, "err", r.err)
			if !g.FailOpen && unavailableErr == nil {
				unavailableErr = fmt.Errorf("admission webhook %s: %w", r.url, r.err)
			}
		case !r.allowed && !denied:
			denied = true
			denyReason = r.reason
		}
	}
	if denied {
		return false, denyReason, nil
	}
	if unavailableErr != nil {
		return false, "", unavailableErr
	}
	return true, "", nil
}

func (g *Gate) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return DefaultTimeout
}

func (g *Gate) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

func (g *Gate) call(ctx context.Context, url string, payload []byte) (allowed bool, reason string, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client().Do(httpReq)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "", fmt.Errorf("decode response: %w", err)
	}
	return out.Allowed, out.Reason, nil
}
