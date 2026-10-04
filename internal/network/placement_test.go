package network

import (
	"context"
	"testing"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	"github.com/kiyuta1230/kyuusha/internal/resourcetest"
)

func TestInterfaceHypervisorFollowsRunningVM(t *testing.T) {
	for _, tc := range []struct {
		phase, hypervisor, want string
	}{
		{"Running", "hv-1", "hv-1"},
		{"Scheduled", "hv-1", ""},
		{"Stopped", "hv-1", ""},
		{"Migrating", "hv-2", ""},
	} {
		vm := &computev1.VirtualMachine{Status: &computev1.VirtualMachineStatus{Phase: tc.phase, Hypervisor: tc.hypervisor}}
		if got := interfaceHypervisor(vm); got != tc.want {
			t.Errorf("phase=%s hypervisor=%s: got %q, want %q", tc.phase, tc.hypervisor, got, tc.want)
		}
	}
}

func TestService_SyncInterfaceHypervisor(t *testing.T) {
	ctx := context.Background()
	svc, err := NewService(ctx, resourcetest.Client(t), &FakeTenantClient{}, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sn := mustCreateAndAllocateSubnet(t, ctx, svc, "tenant-a", "sn", userSubnet(t, ctx, svc, "tenant-a", "zone-a", "10.0.1.0/24", ""))
	mine := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "mine", NetworkInterfaceSpec{VMID: "vm-1", SubnetID: sn.Meta.ID}, sn)
	other := mustCreateAndAllocateNetworkInterface(t, ctx, svc, "tenant-a", "other", NetworkInterfaceSpec{VMID: "vm-2", SubnetID: sn.Meta.ID}, sn)

	svc.syncInterfaceHypervisor(ctx, "tenant-a", "vm-1", "hv-1")
	got, _ := svc.GetNetworkInterface(ctx, "tenant-a", mine.Meta.ID)
	if got.Status.Hypervisor != "hv-1" || got.Status.IPAddress != mine.Status.IPAddress {
		t.Fatalf("vm-1's interface = %+v, want hypervisor hv-1 with its IP untouched", got.Status)
	}
	if o, _ := svc.GetNetworkInterface(ctx, "tenant-a", other.Meta.ID); o.Status.Hypervisor != "" {
		t.Fatalf("vm-2's interface hypervisor = %q, want untouched", o.Status.Hypervisor)
	}

	// A migration: Migrating clears it, Running on the new host sets it.
	svc.syncInterfaceHypervisor(ctx, "tenant-a", "vm-1", "")
	svc.syncInterfaceHypervisor(ctx, "tenant-a", "vm-1", "hv-2")
	if got, _ := svc.GetNetworkInterface(ctx, "tenant-a", mine.Meta.ID); got.Status.Hypervisor != "hv-2" {
		t.Fatalf("after migration hypervisor = %q, want hv-2", got.Status.Hypervisor)
	}
}
