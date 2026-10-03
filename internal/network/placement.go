package network

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

// interfaceHypervisor is what a NetworkInterface's status.hypervisor should
// read given its VM's current state: the VM's Hypervisor while it's
// Running (compute-agent wires every tap during Boot, before reporting
// Running), empty otherwise -- not yet booted, Stopped (taps removed), or
// mid-Migrating (no longer on the old host, not yet on the new one).
func interfaceHypervisor(vm *computev1.VirtualMachine) string {
	if vm.GetStatus().GetPhase() != "Running" {
		return ""
	}
	return vm.GetStatus().GetHypervisor()
}

// syncInterfaceHypervisor sets status.hypervisor on every NetworkInterface
// of vmID (in tenantID) to want, skipping those already there.
func (s *Service) syncInterfaceHypervisor(ctx context.Context, tenantID, vmID, want string) {
	ifaces, err := s.interfaces.List(ctx, tenantID)
	if err != nil {
		slog.Warn("network: list interfaces for hypervisor sync failed", "vm_id", vmID, "err", err)
		return
	}
	for _, n := range ifaces {
		if n.Spec.VMID != vmID {
			continue
		}
		for attempt := 0; attempt < 3 && n.Status.Hypervisor != want && n.Meta.DeletedAt == nil; attempt++ {
			n.Status.Hypervisor = want
			if _, err := s.interfaces.Update(ctx, n); err == nil {
				break
			} else if !errors.Is(err, ErrNetworkInterfaceConflict) {
				slog.Warn("network: update interface hypervisor failed", "netif_id", n.Meta.ID, "err", err)
				break
			}
			current, err := s.interfaces.Get(ctx, n.Meta.TenantID, n.Meta.ID)
			if err != nil {
				break
			}
			n = current
		}
	}
}

// watchVMPlacement keeps NetworkInterface.status.hypervisor in step with
// each VM's placement (see interfaceHypervisor), including a Migrate's
// move between Hypervisors, by watching every tenant's VirtualMachines.
// Resumes from the last seen resource_version after a dropped stream, or
// relists from scratch if that point was compacted away. The orphan sweep
// re-syncs every interface too, as a backstop for anything missed here
// (e.g. an interface created after its VM was already Running).
func (s *Service) watchVMPlacement(ctx context.Context) {
	if s.computeClient == nil {
		return
	}
	last := map[string]string{} // vm_id -> hypervisor last synced, to skip unrelated VM updates
	var sinceRV int64
	for ctx.Err() == nil {
		stream, err := s.computeClient.Watch(ctx, &computev1.WatchVirtualMachinesRequest{SinceResourceVersion: sinceRV})
		if err == nil {
			for {
				ev, rerr := stream.Recv()
				if rerr != nil {
					err = rerr
					break
				}
				sinceRV = ev.GetResourceVersion()
				vm := ev.GetVm()
				id := vm.GetMeta().GetId()
				var want string
				switch ev.GetType() {
				case computev1.VirtualMachineEvent_BOOKMARK:
					continue
				case computev1.VirtualMachineEvent_DELETED:
					delete(last, id)
				default:
					want = interfaceHypervisor(vm)
					if got, ok := last[id]; ok && got == want {
						continue
					}
					last[id] = want
				}
				s.syncInterfaceHypervisor(ctx, vm.GetMeta().GetTenantId(), id, want)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if status.Code(err) == codes.OutOfRange {
			sinceRV = 0 // resume point compacted away: replay current state
		}
		if !errors.Is(err, io.EOF) {
			slog.Warn("network: VM placement watch ended, retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
