package network

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// recordingWebhook records every admission request it receives and denies
// any whose operation+resource is in deny.
type recordingWebhook struct {
	mu   sync.Mutex
	reqs []admissionwebhook.Request
	deny map[string]bool // e.g. "DELETE Subnet"
}

func (w *recordingWebhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	var req admissionwebhook.Request
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.mu.Lock()
	w.reqs = append(w.reqs, req)
	denied := w.deny[req.Operation+" "+req.Resource]
	w.mu.Unlock()
	json.NewEncoder(rw).Encode(admissionwebhook.Response{Allowed: !denied, Reason: "denied by test"})
}

func (w *recordingWebhook) last(t *testing.T) admissionwebhook.Request {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.reqs) == 0 {
		t.Fatal("webhook was never called")
	}
	return w.reqs[len(w.reqs)-1]
}

func TestService_AdmissionWebhookGatesNetworkWrites(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	hook := &recordingWebhook{deny: map[string]bool{}}
	srv := httptest.NewServer(hook)
	defer srv.Close()
	svc.AdmissionGate = admissionwebhook.Gate{URLs: []string{srv.URL}}

	// CREATE carries the proposed labels and spec.
	labels := map[string]string{"vpc.example.com/id": "vpc-1"}
	sn, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "sn", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", "10.0.1.1"), resource.Metadata{Labels: labels})
	if err != nil {
		t.Fatalf("CreateSubnetWithMetadata: %v", err)
	}
	req := hook.last(t)
	if req.Operation != "CREATE" || req.Resource != "Subnet" || req.Labels["vpc.example.com/id"] != "vpc-1" || !strings.Contains(string(req.Spec), `"cidr":"10.0.1.0/24"`) || req.OldObject != nil {
		t.Fatalf("CREATE request = %+v (spec %s)", req, req.Spec)
	}

	// UPDATE carries the new state plus the stored one as old_object.
	svc.tryAllocateSubnet(ctx, sn)
	current, _ := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	current.Meta.Labels = map[string]string{"vpc.example.com/id": "vpc-2"}
	if _, err := svc.UpdateSubnet(ctx, current); err != nil {
		t.Fatalf("UpdateSubnet: %v", err)
	}
	req = hook.last(t)
	if req.Operation != "UPDATE" || req.ID != sn.Meta.ID || req.Labels["vpc.example.com/id"] != "vpc-2" ||
		req.OldObject == nil || req.OldObject.Labels["vpc.example.com/id"] != "vpc-1" || !strings.Contains(string(req.OldObject.Status), `"phase":"Ready"`) {
		t.Fatalf("UPDATE request = %+v", req)
	}

	// SetSecurityGroups is an UPDATE of the NetworkInterface, old groups in old_object.
	ready, _ := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	n := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "netif", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, ready)
	if req := hook.last(t); req.Operation != "CREATE" || req.Resource != "NetworkInterface" {
		t.Fatalf("NetworkInterface CREATE request = %+v", req)
	}
	web, err := svc.CreateSecurityGroup(ctx, "tenant-a", "web", SecurityGroupSpec{IngressRules: []SecurityGroupRule{{Protocol: "tcp", PortRange: "22", Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}}}}, resource.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if req := hook.last(t); req.Operation != "CREATE" || req.Resource != "SecurityGroup" || !strings.Contains(string(req.Spec), `"port_range":"22"`) {
		t.Fatalf("SecurityGroup CREATE request = %+v (spec %s)", req, req.Spec)
	}
	if _, err := svc.SetSecurityGroups(ctx, "tenant-a", n.Meta.ID, []string{web.Meta.ID}); err != nil {
		t.Fatalf("SetSecurityGroups: %v", err)
	}
	req = hook.last(t)
	if req.Operation != "UPDATE" || req.Resource != "NetworkInterface" || !strings.Contains(string(req.Spec), web.Meta.ID) ||
		req.OldObject == nil || strings.Contains(string(req.OldObject.Spec), web.Meta.ID) {
		t.Fatalf("SetSecurityGroups request = %+v (spec %s)", req, req.Spec)
	}

	// A denied DELETE leaves the Subnet in place.
	hook.deny["DELETE Subnet"] = true
	if err := svc.DeleteSubnet(ctx, "tenant-a", sn.Meta.ID); !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("DeleteSubnet with a denying webhook: got %v, want ErrAdmissionDenied", err)
	}
	if req := hook.last(t); req.Operation != "DELETE" || len(req.Spec) != 0 || req.OldObject == nil || req.OldObject.ID != sn.Meta.ID {
		t.Fatalf("DELETE request = %+v", req)
	}
	if _, err := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID); err != nil {
		t.Fatalf("Subnet gone after a denied Delete: %v", err)
	}

	// A denied CREATE creates nothing.
	hook.deny["CREATE Subnet"] = true
	if _, err := svc.CreateSubnet(ctx, "tenant-a", "sn-2", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.2.0/24", "")); !errors.Is(err, ErrAdmissionDenied) {
		t.Fatalf("CreateSubnet with a denying webhook: got %v, want ErrAdmissionDenied", err)
	}
	all, _ := svc.ListSubnets(ctx, "tenant-a")
	if len(all) != 1 {
		t.Fatalf("got %d Subnets after a denied Create, want 1", len(all))
	}
}
