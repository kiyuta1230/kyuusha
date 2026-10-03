package network

import (
	"context"
	"errors"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

// TestService_SubnetLabelsAndAnnotations covers meta.labels/annotations'
// lifecycle on Subnet: set at Create, replaced wholesale by Update, and
// validated on both paths (see resource.ValidateMetadata).
func TestService_SubnetLabelsAndAnnotations(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	md := resource.Metadata{
		Labels:      map[string]string{"vpc.example.com/id": "vpc-1"},
		Annotations: map[string]string{"example.com/note": "anything at all"},
	}
	sn, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "sn", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"}, md)
	if err != nil {
		t.Fatalf("CreateSubnetWithMetadata: %v", err)
	}
	got, err := svc.GetSubnet(ctx, "tenant-a", sn.Meta.ID)
	if err != nil {
		t.Fatalf("GetSubnet: %v", err)
	}
	if got.Meta.Labels["vpc.example.com/id"] != "vpc-1" || got.Meta.Annotations["example.com/note"] != "anything at all" {
		t.Fatalf("stored meta = %+v, want the labels/annotations given at Create", got.Meta)
	}

	got.Meta.Labels = map[string]string{"tier": "web"}
	updated, err := svc.UpdateSubnet(ctx, got)
	if err != nil {
		t.Fatalf("UpdateSubnet: %v", err)
	}
	if len(updated.Meta.Labels) != 1 || updated.Meta.Labels["tier"] != "web" {
		t.Fatalf("labels after Update = %v, want exactly {tier: web}", updated.Meta.Labels)
	}

	updated.Meta.Labels = map[string]string{"bad key": "x"}
	if _, err := svc.UpdateSubnet(ctx, updated); !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateSubnet with an invalid label key: got %v, want ErrValidation", err)
	}
	if _, err := svc.CreateSubnetWithMetadata(ctx, "tenant-a", "sn-bad", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"},
		resource.Metadata{Labels: map[string]string{"k": "has space"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateSubnetWithMetadata with an invalid label value: got %v, want ErrValidation", err)
	}
}
