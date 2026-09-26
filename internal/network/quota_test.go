package network

import (
	"context"
	"errors"
	"testing"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestService_CreateSubnetEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{MaxSubnets: 2}}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	first, err := svc.CreateSubnet(ctx, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("first CreateSubnet: %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, tenant, "subnet-2", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"}); err != nil {
		t.Fatalf("second CreateSubnet (within quota): %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, tenant, "subnet-3", SubnetSpec{Zone: "zone-a", CIDR: "10.0.3.0/24"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("third CreateSubnet over max_subnets=2: got %v, want ErrQuotaExceeded", err)
	}

	// Idempotent re-Create of an existing name must not re-charge quota or
	// be rejected by the already-exhausted quota.
	again, err := svc.CreateSubnet(ctx, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if err != nil {
		t.Fatalf("idempotent re-CreateSubnet: %v", err)
	}
	if again.Meta.ID != first.Meta.ID {
		t.Fatalf("idempotent re-Create minted a new ID: %s vs %s", again.Meta.ID, first.Meta.ID)
	}

	// Deleting one Subnet frees enough quota for another.
	if err := svc.DeleteSubnet(ctx, tenant, first.Meta.ID); err != nil {
		t.Fatalf("DeleteSubnet: %v", err)
	}
	if _, err := svc.CreateSubnet(ctx, tenant, "subnet-4", SubnetSpec{Zone: "zone-a", CIDR: "10.0.4.0/24"}); err != nil {
		t.Fatalf("CreateSubnet after DeleteSubnet freed quota: %v", err)
	}
}

func TestService_CreateNetworkInterfaceEnforcesQuota(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{Quota: &identityv1.QuotaSpec{MaxSubnets: 1, MaxNetworkInterfaces: 2}}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const tenant = "tenant-a"

	subnet := mustCreateAndAllocateSubnet(t, ctx, svc, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})

	first, err := svc.CreateNetworkInterface(ctx, tenant, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnet.Meta.ID})
	if err != nil {
		t.Fatalf("first CreateNetworkInterface: %v", err)
	}
	if _, err := svc.CreateNetworkInterface(ctx, tenant, "netif-2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: subnet.Meta.ID}); err != nil {
		t.Fatalf("second CreateNetworkInterface (within quota): %v", err)
	}
	if _, err := svc.CreateNetworkInterface(ctx, tenant, "netif-3", NetworkInterfaceSpec{VMID: "vm-3", SubnetID: subnet.Meta.ID}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("third CreateNetworkInterface over max_network_interfaces=2: got %v, want ErrQuotaExceeded", err)
	}

	// Deleting one NetworkInterface frees enough quota for another.
	if err := svc.DeleteNetworkInterface(ctx, tenant, first.Meta.ID); err != nil {
		t.Fatalf("DeleteNetworkInterface: %v", err)
	}
	if _, err := svc.CreateNetworkInterface(ctx, tenant, "netif-4", NetworkInterfaceSpec{VMID: "vm-4", SubnetID: subnet.Meta.ID}); err != nil {
		t.Fatalf("CreateNetworkInterface after DeleteNetworkInterface freed quota: %v", err)
	}
}

// TestService_NewServiceRebuildsUsageFromExistingSubnetsAndNetworkInterfaces
// proves the same class of bug found 2026-09-13 in rebuildPools/compute/
// block-storage/image's identical rebuilds: usage is purely in-memory, so
// without rebuildUsage, a restart forgets every tenant's real usage.
// Constructs two Services against the SAME etcd client/namespace (not
// resourcetest.Client(t) called twice, which would give each its own
// isolated namespace) to simulate a real restart.
func TestService_NewServiceRebuildsUsageFromExistingSubnetsAndNetworkInterfaces(t *testing.T) {
	ctx := context.Background()
	etcdClient := resourcetest.Client(t)
	quota := &identityv1.QuotaSpec{MaxSubnets: 1, MaxNetworkInterfaces: 1}
	const tenant = "tenant-a"

	svc1, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, nil, nil)
	if err != nil {
		t.Fatalf("NewService (first): %v", err)
	}
	subnet := mustCreateAndAllocateSubnet(t, ctx, svc1, tenant, "subnet-1", SubnetSpec{Zone: "zone-a", CIDR: "10.0.1.0/24"})
	if _, err := svc1.CreateNetworkInterface(ctx, tenant, "netif-1", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: subnet.Meta.ID}); err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}

	svc2, err := NewService(ctx, etcdClient, &FakeTenantClient{Quota: quota}, nil, nil)
	if err != nil {
		t.Fatalf("NewService (second, simulating a restart): %v", err)
	}

	if _, err := svc2.CreateSubnet(ctx, tenant, "subnet-2", SubnetSpec{Zone: "zone-a", CIDR: "10.0.2.0/24"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("CreateSubnet (after restart, over max_subnets=1): got %v, want ErrQuotaExceeded -- rebuildUsage did not restore usage's state", err)
	}
	if _, err := svc2.CreateNetworkInterface(ctx, tenant, "netif-2", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: subnet.Meta.ID}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("CreateNetworkInterface (after restart, over max_network_interfaces=1): got %v, want ErrQuotaExceeded -- rebuildUsage did not restore usage's state", err)
	}
}
