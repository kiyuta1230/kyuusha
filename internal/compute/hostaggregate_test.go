package compute

import (
	"context"
	"errors"
	"testing"
)

func TestService_ScheduleVMFiltersByHostAggregate(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	for _, h := range []struct{ id, zone string }{{"hv-1", "zone-a"}, {"hv-2", "zone-a"}, {"hv-3", "zone-b"}} {
		if _, err := svc.RegisterHypervisor(ctx, h.id, h.zone, 8, 16384, []string{"FIRECRACKER"}, nil, nil, nil); err != nil {
			t.Fatalf("Register %s: %v", h.id, err)
		}
	}
	// hv-3 is listed but in zone-b: an aggregate only counts for members in its own zone.
	if _, err := svc.CreateHostAggregate(ctx, "leaf-1", HostAggregateSpec{Zone: "zone-a", Labels: map[string]string{"leaf": "1", "tier": "gold"}, Hypervisors: []string{"hv-2", "hv-3"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateHostAggregate(ctx, "ssd", HostAggregateSpec{Zone: "zone-a", Labels: map[string]string{"disk": "ssd"}, Hypervisors: []string{"hv-1", "hv-2"}}); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		picked, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 512}, scheduleConstraints{AggregateSelectors: []map[string]string{{"leaf": "1"}}})
		if err != nil || picked != "hv-2" {
			t.Fatalf("round %d: picked %q, err %v; want hv-2 (the only zone-a member of leaf=1)", i, picked, err)
		}
	}
	// Two selectors may be met by two different aggregates.
	picked, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 512}, scheduleConstraints{AggregateSelectors: []map[string]string{{"leaf": "1"}, {"disk": "ssd"}}})
	if err != nil || picked != "hv-2" {
		t.Fatalf("leaf=1 + disk=ssd: picked %q, err %v; want hv-2", picked, err)
	}
	if _, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 512}, scheduleConstraints{Zone: "zone-b", AggregateSelectors: []map[string]string{{"leaf": "1"}}}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("zone-b with leaf=1: got %v, want ErrUnschedulable", err)
	}
	if _, _, _, err := svc.scheduleVM(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 512}, scheduleConstraints{AggregateSelectors: []map[string]string{{"leaf": "1", "tier": "silver"}}}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("label value mismatch: got %v, want ErrUnschedulable", err)
	}

	// Migration: auto-pick and explicit target obey the same filter.
	if _, _, _, err := svc.scheduleMigration(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 512}, "hv-2", "", scheduleConstraints{AggregateSelectors: []map[string]string{{"leaf": "1"}}}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("migrate off the only member: got %v, want ErrUnschedulable", err)
	}
	if _, _, _, err := svc.scheduleMigration(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 512}, "hv-2", "hv-1", scheduleConstraints{AggregateSelectors: []map[string]string{{"leaf": "1"}}}); !errors.Is(err, ErrUnschedulable) {
		t.Fatalf("migrate to a non-member target: got %v, want ErrUnschedulable", err)
	}
	if got, _, _, err := svc.scheduleMigration(ctx, VirtualMachineSpec{VCPU: 1, MemoryMB: 512}, "hv-2", "hv-1", scheduleConstraints{AggregateSelectors: []map[string]string{{"disk": "ssd"}}}); err != nil || got != "hv-1" {
		t.Fatalf("migrate to a member target: got %q, %v", got, err)
	}
}

func TestService_HostAggregateCRUD(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, ctx)
	if _, err := svc.CreateHostAggregate(ctx, "x", HostAggregateSpec{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing zone: got %v", err)
	}
	if _, err := svc.CreateHostAggregate(ctx, "x", HostAggregateSpec{Zone: "z", Hypervisors: []string{"a", "a"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicate member: got %v", err)
	}
	a, err := svc.CreateHostAggregate(ctx, "x", HostAggregateSpec{Zone: "z", Hypervisors: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Spec.Hypervisors = append(a.Spec.Hypervisors, "b")
	if a, err = svc.UpdateHostAggregate(ctx, a); err != nil || len(a.Spec.Hypervisors) != 2 {
		t.Fatalf("update: %+v, %v", a, err)
	}
	if err := svc.DeleteHostAggregate(ctx, a.Meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetHostAggregate(ctx, a.Meta.ID); !errors.Is(err, ErrHostAggregateNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestAggregateSelectors_FromNetworkClass(t *testing.T) {
	ctx := t.Context()
	s := &Service{subnetClient: &FakeSubnetClient{}, NetworkClient: &FakeNetworkClient{}}
	if sel, err := s.aggregateSelectors(ctx, "t", []NetworkAttachment{{NetworkID: "network-1"}}); err != nil || sel != nil {
		t.Fatalf("without a class client: %v, %v; want no constraint", sel, err)
	}
	s.NetworkClassClient = &FakeNetworkClassClient{HostAggregateSelector: map[string]string{"leaf": "1"}}
	// Two NICs on the same Network yield the selector once; a pinned Subnet resolves via its network_id.
	sel, err := s.aggregateSelectors(ctx, "t", []NetworkAttachment{{NetworkID: "network-1"}, {NetworkID: "network-1"}, {SubnetID: "subnet-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) < 1 || sel[0]["leaf"] != "1" {
		t.Fatalf("selectors = %v", sel)
	}
	if _, err := s.aggregateSelectors(ctx, "t", []NetworkAttachment{{NetworkID: "network-missing"}}); err == nil {
		t.Fatal("missing network: want error")
	}
}
