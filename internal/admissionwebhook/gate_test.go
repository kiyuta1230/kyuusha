package admissionwebhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func allowServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Allowed: true})
	}))
}

func denyServer(t *testing.T, reason string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Response{Allowed: false, Reason: reason})
	}))
}

func TestValidateNoURLsAlwaysAllows(t *testing.T) {
	g := &Gate{}
	allowed, reason, err := g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine"})
	if err != nil || !allowed || reason != "" {
		t.Fatalf("Validate with no URLs = (%v, %q, %v), want (true, \"\", nil)", allowed, reason, err)
	}
}

func TestValidateAllowsWhenEveryWebhookAllows(t *testing.T) {
	s1, s2 := allowServer(t), allowServer(t)
	defer s1.Close()
	defer s2.Close()

	g := &Gate{URLs: []string{s1.URL, s2.URL}}
	allowed, _, err := g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine", TenantID: "t1"})
	if err != nil || !allowed {
		t.Fatalf("Validate = (%v, err=%v), want (true, nil)", allowed, err)
	}
}

func TestValidateDeniesWhenAnyWebhookDenies(t *testing.T) {
	allow, deny := allowServer(t), denyServer(t, "policy X forbids this")
	defer allow.Close()
	defer deny.Close()

	g := &Gate{URLs: []string{allow.URL, deny.URL}}
	allowed, reason, err := g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine"})
	if err != nil {
		t.Fatalf("Validate returned an error for an explicit deny: %v", err)
	}
	if allowed {
		t.Fatal("Validate allowed despite one webhook denying")
	}
	if reason != "policy X forbids this" {
		t.Fatalf("reason = %q, want the denying webhook's reason", reason)
	}
}

func TestValidateSendsCorrectRequestBody(t *testing.T) {
	var got Request
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(Response{Allowed: true})
	}))
	defer s.Close()

	g := &Gate{URLs: []string{s.URL}}
	spec := json.RawMessage(`{"vcpu":2,"memory_mb":1024}`)
	_, _, err := g.Validate(context.Background(), Request{
		Operation: "CREATE", Resource: "VirtualMachine", TenantID: "tenant-a", Name: "web-1", Spec: spec,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.Operation != "CREATE" || got.Resource != "VirtualMachine" || got.TenantID != "tenant-a" || got.Name != "web-1" {
		t.Fatalf("webhook received %+v, want operation/resource/tenant_id/name to match", got)
	}
	if string(got.Spec) != string(spec) {
		t.Fatalf("webhook received spec %s, want %s", got.Spec, spec)
	}
}

func TestValidateFailClosedOnUnreachableWebhook(t *testing.T) {
	g := &Gate{URLs: []string{"http://127.0.0.1:1"}, Timeout: 500 * time.Millisecond} // nothing listens on port 1
	allowed, _, err := g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine"})
	if err == nil {
		t.Fatal("Validate against an unreachable webhook: got nil error, want non-nil (fail-closed)")
	}
	if allowed {
		t.Fatal("Validate allowed despite an unreachable webhook and FailOpen=false")
	}
}

func TestValidateFailOpenOnUnreachableWebhook(t *testing.T) {
	g := &Gate{URLs: []string{"http://127.0.0.1:1"}, Timeout: 500 * time.Millisecond, FailOpen: true}
	allowed, _, err := g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine"})
	if err != nil {
		t.Fatalf("Validate with FailOpen=true: got error %v, want nil", err)
	}
	if !allowed {
		t.Fatal("Validate denied despite FailOpen=true")
	}
}

// TestValidateExplicitDenyBeatsFailOpen confirms an explicit deny from one
// webhook is never overridden by FailOpen just because a *different*
// webhook happened to be unreachable at the same time.
func TestValidateExplicitDenyBeatsFailOpen(t *testing.T) {
	deny := denyServer(t, "explicit no")
	defer deny.Close()

	g := &Gate{URLs: []string{deny.URL, "http://127.0.0.1:1"}, Timeout: 500 * time.Millisecond, FailOpen: true}
	allowed, reason, err := g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine"})
	if err != nil {
		t.Fatalf("Validate: got error %v, want nil (explicit deny, not an infra failure)", err)
	}
	if allowed {
		t.Fatal("Validate allowed despite an explicit deny, even with FailOpen=true")
	}
	if reason != "explicit no" {
		t.Fatalf("reason = %q, want the explicit deny's reason", reason)
	}
}

func TestValidateCallsAllURLsConcurrently(t *testing.T) {
	var calls int32
	block := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-block // held open until every request has actually arrived
		json.NewEncoder(w).Encode(Response{Allowed: true})
	}))
	defer s.Close()

	g := &Gate{URLs: []string{s.URL, s.URL, s.URL}}
	done := make(chan struct{})
	go func() {
		g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine"})
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d/3 concurrent calls arrived before deadline (want all 3 in flight at once)", atomic.LoadInt32(&calls))
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(block)
	<-done
}

func TestValidateTimeout(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		json.NewEncoder(w).Encode(Response{Allowed: true})
	}))
	defer s.Close()

	g := &Gate{URLs: []string{s.URL}, Timeout: 10 * time.Millisecond}
	allowed, _, err := g.Validate(context.Background(), Request{Operation: "CREATE", Resource: "VirtualMachine"})
	if err == nil {
		t.Fatal("Validate against a slow webhook with a short Timeout: got nil error, want a timeout error")
	}
	if allowed {
		t.Fatal("Validate allowed despite a timeout")
	}
}
