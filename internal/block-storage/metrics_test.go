package blockstorage

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetricsCollector_CountsAndQuotaUsage covers the two things a real bug
// could plausibly get wrong: mixing two tenants' counts/usage together, and
// dropping the phase/resource label. Both Volumes and the VolumeAttachment
// stay Pending (Create never attaches/verifies synchronously post-split --
// see internal/block-storage/service.go), so this also implicitly proves
// the collector doesn't require the reconcile loop to actually be running.
func TestMetricsCollector_CountsAndQuotaUsage(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)

	volA, err := svc.CreateVolume(ctx, "tenant-a", "vol-1", testVolumeSpec(10))
	if err != nil {
		t.Fatalf("CreateVolume vol-1: %v", err)
	}
	volA = forceVolumeVerified(t, ctx, svc, volA) // CreateVolumeAttachment requires an already-Ready Volume
	if _, err := svc.CreateVolume(ctx, "tenant-a", "vol-2", testVolumeSpec(20)); err != nil {
		t.Fatalf("CreateVolume vol-2: %v", err)
	}
	if _, err := svc.CreateVolume(ctx, "tenant-b", "vol-3", testVolumeSpec(30)); err != nil {
		t.Fatalf("CreateVolume vol-3: %v", err)
	}
	if _, err := svc.CreateVolumeAttachment(ctx, "tenant-a", "attach-1", VolumeAttachmentSpec{VMID: "vm-1", VolumeID: volA.Meta.ID}); err != nil {
		t.Fatalf("CreateVolumeAttachment: %v", err)
	}

	c := NewMetricsCollector(svc)
	want := `
# HELP kyuusha_tenant_quota_used block-storage's own tenant_usage accounting (see docs/architecture.md「Quota設計」), by tenant_id and resource (volume_gb). The matching limit is exposed by identity as kyuusha_tenant_quota_limit.
# TYPE kyuusha_tenant_quota_used gauge
kyuusha_tenant_quota_used{resource="volume_gb",tenant_id="tenant-a"} 30
kyuusha_tenant_quota_used{resource="volume_gb",tenant_id="tenant-b"} 30
# HELP kyuusha_volumeattachments_total Current number of VolumeAttachment objects, by tenant_id and phase.
# TYPE kyuusha_volumeattachments_total gauge
kyuusha_volumeattachments_total{phase="Pending",tenant_id="tenant-a"} 1
# HELP kyuusha_volumes_total Current number of Volume objects, by tenant_id and phase.
# TYPE kyuusha_volumes_total gauge
kyuusha_volumes_total{phase="Pending",tenant_id="tenant-a"} 1
kyuusha_volumes_total{phase="Pending",tenant_id="tenant-b"} 1
kyuusha_volumes_total{phase="Ready",tenant_id="tenant-a"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected collector output: %v", err)
	}
}
