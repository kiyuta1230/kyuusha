package compute

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
)

func TestService_CreateAllowedByAdmissionWebhook(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(admissionwebhook.Response{Allowed: true})
	}))
	defer s.Close()
	svc.AdmissionGate = admissionwebhook.Gate{URLs: []string{s.URL}}

	vm, err := svc.Create(ctx, "tenant-a", "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if vm.Status.Phase != PhasePending {
		t.Fatalf("phase = %q, want Pending", vm.Status.Phase)
	}
}

func TestService_CreateDeniedByAdmissionWebhookIsNotPersistedOrCharged(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	var received admissionwebhook.Request
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(admissionwebhook.Response{Allowed: false, Reason: "no GPUs on Tuesdays"})
	}))
	defer s.Close()
	svc.AdmissionGate = admissionwebhook.Gate{URLs: []string{s.URL}}

	_, err := svc.Create(ctx, "tenant-a", "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 2, MemoryMB: 1024})
	if !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("Create: got %v, want ErrAdmissionDenied", err)
	}
	if !strings.Contains(err.Error(), "no GPUs on Tuesdays") {
		t.Fatalf("error %v does not include the webhook's reason", err)
	}

	if received.Operation != "CREATE" || received.Resource != "VirtualMachine" || received.TenantID != "tenant-a" || received.Name != "web-1" {
		t.Fatalf("webhook received %+v, want operation/resource/tenant_id/name populated", received)
	}
	var spec admissionVMSpec
	if err := json.Unmarshal(received.Spec, &spec); err != nil {
		t.Fatalf("unmarshal received spec: %v", err)
	}
	if spec.ImageID != "img-abc" || spec.VCPU != 2 || spec.MemoryMB != 1024 {
		t.Fatalf("webhook received spec %+v, want it to match the Create request", spec)
	}

	// A denied Create must be a true no-op: nothing persisted, nothing
	// charged against tenant_usage (a later Create for the same name must
	// not be treated as an idempotent replay of a VM that doesn't exist).
	if _, ok := svc.store.LookupByName(ctx, "tenant-a", "web-1"); ok {
		t.Fatal("a denied Create left a VirtualMachine behind")
	}
	if u := svc.usage["tenant-a"]; u.VCPU != 0 || u.MemoryMB != 0 || u.VMCount != 0 {
		t.Fatalf("tenant_usage after a denied Create = %+v, want all zero", u)
	}
}

func TestService_CreateFailsClosedWhenWebhookUnreachable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	svc.AdmissionGate = admissionwebhook.Gate{URLs: []string{"http://127.0.0.1:1"}}

	_, err := svc.Create(ctx, "tenant-a", "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if !errors.Is(err, ErrAdmissionUnavailable) {
		t.Fatalf("Create with an unreachable webhook: got %v, want ErrAdmissionUnavailable", err)
	}
}

func TestService_CreateFailsOpenWhenConfigured(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	svc.AdmissionGate = admissionwebhook.Gate{URLs: []string{"http://127.0.0.1:1"}, FailOpen: true}

	vm, err := svc.Create(ctx, "tenant-a", "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create with FailOpen and an unreachable webhook: %v", err)
	}
	if vm.Status.Phase != PhasePending {
		t.Fatalf("phase = %q, want Pending", vm.Status.Phase)
	}
}

func TestService_CreateNoAdmissionGateConfiguredIsUnaffected(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx) // AdmissionGate left at its zero value

	if _, err := svc.Create(ctx, "tenant-a", "web-1", VirtualMachineSpec{ImageID: "img-abc", VCPU: 1, MemoryMB: 512}); err != nil {
		t.Fatalf("Create with no AdmissionGate configured: %v", err)
	}
}
