package image

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestMetricsCollector_CountsByTenantAndPhase covers the two things a real
// bug could plausibly get wrong: mixing two tenants' counts together, and
// dropping the phase label. Both Images stay Pending here (Run's
// reachability checker never runs in this test), which also implicitly
// proves the collector doesn't require it to be running.
func TestMetricsCollector_CountsByTenantAndPhase(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := svc.Create(ctx, "tenant-a", "img-1", kernelRootfsSpec()); err != nil {
		t.Fatalf("Create img-1: %v", err)
	}
	if _, err := svc.Create(ctx, "tenant-a", "img-2", kernelRootfsSpec()); err != nil {
		t.Fatalf("Create img-2: %v", err)
	}
	if _, err := svc.Create(ctx, "tenant-b", "img-3", kernelRootfsSpec()); err != nil {
		t.Fatalf("Create img-3: %v", err)
	}

	c := NewMetricsCollector(svc)
	want := `
# HELP kyuusha_images_total Current number of Image objects, by tenant_id and phase.
# TYPE kyuusha_images_total gauge
kyuusha_images_total{phase="Pending",tenant_id="tenant-a"} 2
kyuusha_images_total{phase="Pending",tenant_id="tenant-b"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}
