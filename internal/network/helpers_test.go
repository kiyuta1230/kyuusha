package network

import (
	"context"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// testNetwork returns tenantID's Ready test Network: a public NetworkClass
// whose every zone takes a user-specified CIDR (USER_ANY), standing in for
// the old "Subnet carries its own CIDR" shape most tests only need.
// Idempotent per (svc, tenantID): pool/class/network are all created by
// name.
func testNetwork(t *testing.T, ctx context.Context, svc *Service, tenantID string) string {
	t.Helper()
	pool, err := svc.CreateAllocationPool(ctx, "test-user-cidr", AllocationPoolSpec{Cidr: &CidrPoolSpec{Mode: CidrModeUserAny}}, resource.Metadata{})
	if err != nil {
		t.Fatalf("CreateAllocationPool: %v", err)
	}
	class, err := svc.CreateNetworkClass(ctx, "test-user", NetworkClassSpec{
		Subnet:     map[string][]PoolRef{"*": {{PoolID: pool.Meta.ID}}},
		Visibility: VisibilityPublic,
	}, resource.Metadata{})
	if err != nil {
		t.Fatalf("CreateNetworkClass: %v", err)
	}
	return mustReadyNetwork(t, ctx, svc, tenantID, "test-net", NetworkSpec{NetworkClass: class.Meta.ID})
}

// mustReadyNetwork creates a Network and drives it through allocation the
// way network-reconciler's watch would, failing the test unless it ends
// up Ready.
func mustReadyNetwork(t *testing.T, ctx context.Context, svc *Service, tenantID, name string, spec NetworkSpec) string {
	t.Helper()
	n, err := svc.CreateNetwork(ctx, tenantID, name, spec, resource.Metadata{})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if n.Status.Phase != NetworkPhaseReady {
		svc.tryAllocateNetwork(ctx, n)
	}
	if n.Status.Phase != NetworkPhaseReady {
		t.Fatalf("network %s not Ready: %+v", name, n.Status)
	}
	return n.Meta.ID
}

// userSubnet is a SubnetSpec on tenantID's test Network with a requested
// CIDR (and optional gateway).
func userSubnet(t *testing.T, ctx context.Context, svc *Service, tenantID, zone, cidr, gateway string) SubnetSpec {
	t.Helper()
	return SubnetSpec{
		NetworkID:          testNetwork(t, ctx, svc, tenantID),
		Zone:               zone,
		RequestedAddresses: []SubnetAddress{{CIDR: cidr, GatewayIP: gateway}},
	}
}

// mustCreateAndAllocateSubnet creates a Subnet and immediately drives it
// through the allocation attempt cmd/network-reconciler's
// watchPendingSubnets would make on its Added event -- CreateSubnet itself
// never allocates (the allocator is process-local state, unsafe to touch
// from a possibly multi-replica API handler), so a test that needs a
// Subnet actually Ready triggers the attempt explicitly, standing in for
// the real reconciler process.
func mustCreateAndAllocateSubnet(t *testing.T, ctx context.Context, svc *Service, tenantID, name string, spec SubnetSpec) *Subnet {
	t.Helper()
	sn, err := svc.CreateSubnet(ctx, tenantID, name, spec)
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	svc.tryAllocateSubnet(ctx, sn)
	return sn
}

// mustCreateAndAllocateNetworkInterface mirrors mustCreateAndAllocateSubnet,
// for ip_address/mac_address allocation via tryAllocateIP. subnet is
// unused (tryAllocateIP resolves it) and kept for call-site readability.
func mustCreateAndAllocateNetworkInterface(t *testing.T, ctx context.Context, svc *Service, tenantID, name string, spec NetworkInterfaceSpec, _ *Subnet) *NetworkInterface {
	t.Helper()
	n, err := svc.CreateNetworkInterface(ctx, tenantID, name, spec)
	if err != nil {
		t.Fatalf("CreateNetworkInterface: %v", err)
	}
	svc.tryAllocateIP(ctx, n)
	return n
}

func subnetIPv4(sn *Subnet) (string, string) { return sn.Status.IPv4() }
