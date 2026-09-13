package identity

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// byTenantID orders two tenant_id label values the same way
// prometheus.Registry.Gather() canonically sorts a metric family's series
// (ascending by the fully-qualified label set) -- since "resource" always
// sorts before "tenant_id" and both fixtures here share every resource
// label, the tenant_id ordering below is the only thing that varies, and
// it must match this to avoid CollectAndCompare's literal text diff
// failing on nothing more than which randomly-generated tenant_id happens
// to sort first.
func byTenantID(a, b string) (first, second string) {
	if a < b {
		return a, b
	}
	return b, a
}

// TestMetricsCollector_ExposesEachTenantsQuotaLimit covers the two things a
// real bug could plausibly get wrong: mixing two tenants' limits together,
// and dropping one of the four resource labels.
func TestMetricsCollector_ExposesEachTenantsQuotaLimit(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	acme, err := svc.Create(ctx, "acme", TenantSpec{
		DisplayName: "Acme Corp",
		Quota:       QuotaSpec{MaxVCPU: 8, MaxMemoryMB: 16384, MaxVolumeGB: 100, MaxVMs: 10},
	})
	if err != nil {
		t.Fatalf("Create acme: %v", err)
	}
	widget, err := svc.Create(ctx, "widget", TenantSpec{
		DisplayName: "Widget Inc",
		Quota:       QuotaSpec{MaxVCPU: 4, MaxMemoryMB: 8192, MaxVolumeGB: 50, MaxVMs: 5},
	})
	if err != nil {
		t.Fatalf("Create widget: %v", err)
	}

	// memory_mb/vms/volume_gb values below are keyed to whichever of
	// acme/widget's randomly-generated tenant_id sorts first (see
	// byTenantID) -- acme always carries the larger quota values in this
	// fixture, so map each sorted position back to its actual limits.
	limitsByID := map[string][4]int64{
		acme.Meta.ID:   {16384, 8, 10, 100}, // memory_mb, vcpu, vms, volume_gb
		widget.Meta.ID: {8192, 4, 5, 50},
	}
	first, second := byTenantID(acme.Meta.ID, widget.Meta.ID)
	f, s := limitsByID[first], limitsByID[second]

	c := NewMetricsCollector(svc)
	want := "" +
		"# HELP kyuusha_tenant_quota_limit Each Tenant's own Quota limit, by tenant_id and resource (vcpu|memory_mb|volume_gb|vms). The matching usage is exposed by compute (vcpu|memory_mb|vms) and block-storage (volume_gb) as kyuusha_tenant_quota_used.\n" +
		"# TYPE kyuusha_tenant_quota_limit gauge\n" +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"memory_mb\",tenant_id=%q} %d\n", first, f[0]) +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"memory_mb\",tenant_id=%q} %d\n", second, s[0]) +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"vcpu\",tenant_id=%q} %d\n", first, f[1]) +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"vcpu\",tenant_id=%q} %d\n", second, s[1]) +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"vms\",tenant_id=%q} %d\n", first, f[2]) +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"vms\",tenant_id=%q} %d\n", second, s[2]) +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"volume_gb\",tenant_id=%q} %d\n", first, f[3]) +
		fmt.Sprintf("kyuusha_tenant_quota_limit{resource=\"volume_gb\",tenant_id=%q} %d\n", second, s[3])
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "kyuusha_tenant_quota_limit"); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}

// TestMetricsCollector_CountsTenantsByPhase covers dropping the phase label
// or double-counting across phases -- Delete moves a Tenant to Deleting
// without actually removing it from the store until finalizers clear (same
// lifecycle shape as every other resource type in this codebase), so a
// real Service can easily have Tenants in more than one phase at once.
func TestMetricsCollector_CountsTenantsByPhase(t *testing.T) {
	ctx := context.Background()
	svc := NewService(resourcetest.Client(t))

	if _, err := svc.Create(ctx, "acme", TenantSpec{DisplayName: "Acme"}); err != nil {
		t.Fatalf("Create acme: %v", err)
	}
	if _, err := svc.Create(ctx, "widget", TenantSpec{DisplayName: "Widget"}); err != nil {
		t.Fatalf("Create widget: %v", err)
	}

	c := NewMetricsCollector(svc)
	want := `
# HELP kyuusha_tenants_total Current number of Tenant objects, by phase.
# TYPE kyuusha_tenants_total gauge
kyuusha_tenants_total{phase="Active"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "kyuusha_tenants_total"); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}
