package network

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestMetricsCollector_CountsByTenantAndPhase covers the two things a real
// bug could plausibly get wrong: mixing two tenants' counts together, and
// dropping the phase label. sn2/nic2 are left Pending (no reconciler
// running here, so tryAllocate* is never called for them), while sn1/nic1
// are driven to Ready via the same helpers other network tests use --
// together this proves both phase values are reported correctly, not just
// whichever one Create alone happens to leave objects in.
func TestMetricsCollector_CountsByTenantAndPhase(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sn1 := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if _, err := svc.CreateSubnet(ctx, "tenant-b", "sn2", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"}); err != nil {
		t.Fatalf("CreateSubnet sn2: %v", err)
	}
	mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "nic1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn1.Meta.ID}, sn1)
	if _, err := svc.CreateNetworkInterface(ctx, "tenant-a", "nic2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn1.Meta.ID}); err != nil {
		t.Fatalf("CreateNetworkInterface nic2: %v", err)
	}

	c := NewMetricsCollector(svc)
	want := `
# HELP kyuusha_networkinterfaces_total Current number of NetworkInterface objects, by tenant_id and phase.
# TYPE kyuusha_networkinterfaces_total gauge
kyuusha_networkinterfaces_total{phase="Pending",tenant_id="tenant-a"} 1
kyuusha_networkinterfaces_total{phase="Ready",tenant_id="tenant-a"} 1
# HELP kyuusha_subnets_total Current number of Subnet objects, by tenant_id and phase.
# TYPE kyuusha_subnets_total gauge
kyuusha_subnets_total{phase="Pending",tenant_id="tenant-b"} 1
kyuusha_subnets_total{phase="Ready",tenant_id="tenant-a"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}
