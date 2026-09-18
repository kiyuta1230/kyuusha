package compute

import (
	"context"
	"errors"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

var (
	ErrHypervisorNotFound      = errors.New("hypervisor: not found")
	ErrHypervisorConflict      = errors.New("hypervisor: resource_version conflict")
	ErrHypervisorHistoryPruned = errors.New("hypervisor: watch resume point too old, relist required")
	ErrUnschedulable           = errors.New("vm: no hypervisor satisfies scheduling constraints")
	ErrHypervisorRevoked       = errors.New("hypervisor: this id has been revoked, registration rejected")
	// ErrHypervisorCapacityExceeded is Resize's counterpart to
	// ErrUnschedulable: unlike scheduling (where no candidate among possibly
	// many fit), a resize is pinned to the VM's already-assigned Hypervisor,
	// so failure specifically means "that one Hypervisor doesn't have room" --
	// distinct enough (for logs/metrics/client messages, and because it maps
	// to ResourceExhausted like quota, not the FailedPrecondition Unschedulable
	// implies) to warrant its own error rather than reusing ErrUnschedulable.
	ErrHypervisorCapacityExceeded = errors.New("vm: hypervisor lacks capacity for resize")
)

// HypervisorEvent is re-exported from the generic resource.Store, distinct
// from VirtualMachine's Event since both live in this package.
type HypervisorEvent = resource.Event[Hypervisor]

// heartbeatTimeout/healthSweepInterval implement docs/architecture.md
// "ハイパーバイザー死活監視とリカバリ": a Hypervisor that hasn't heartbeat
// within heartbeatTimeout is NotReady, filtered out of scheduling.
const (
	heartbeatTimeout    = 15 * time.Second
	healthSweepInterval = 5 * time.Second
)

// RegisterHypervisor is compute-agent's self-registration/re-registration
// (docs/architecture.md "Hypervisor自己登録とzone割当"), called directly by
// compute-agent -- not through api-gateway, since this is east-west, not a
// KaaS-facing operation. Idempotent-upsert: a re-register (e.g. agent
// restart) refreshes zone/capacity/drivers but preserves the in-flight
// allocated_* reservations the scheduler already made against it.
//
// zone itself is verified by the caller (grpcserver.HypervisorServer.Register,
// against a bootstrap token's zone claim -- internal/bootstraptoken) before
// this is ever invoked; this trusts whatever zone it's handed, never the
// agent's own separate claim. See docs/specs/hypervisor-bootstrap.md for
// what's still open beyond that (individual hypervisor identity/revocation
// -- see ErrHypervisorRevoked below -- and single-use tokens).
func (s *Service) RegisterHypervisor(ctx context.Context, hypervisor, zone string, allocatableVCPU int32, allocatableMemoryMB int64, supportedDrivers []string, storageConnections []StorageConnection) (*Hypervisor, error) {
	existing, err := s.hypervisors.Get(ctx, "", hypervisor)
	hadExisting := err == nil
	if hadExisting && existing.Spec.Revoked {
		return nil, ErrHypervisorRevoked
	}

	// New Hypervisors default to schedulable, like a new Kubernetes Node.
	// Schedulable is operator intent (see HypervisorSpec's doc comment) and
	// must never be reset by a re-register, or a restarting agent would
	// silently undo an operator's SetSchedulable(false) maintenance action.
	spec := HypervisorSpec{Schedulable: true}
	if hadExisting {
		spec.Schedulable = existing.Spec.Schedulable
		spec.Revoked = existing.Spec.Revoked // always false here, but explicit: Register never clears it
	}

	status := HypervisorStatus{
		Phase:               HypervisorPhaseReady,
		Zone:                zone,
		LastHeartbeatAt:     time.Now(),
		AllocatableVCPU:     allocatableVCPU,
		AllocatableMemoryMB: allocatableMemoryMB,
		SupportedDrivers:    supportedDrivers,
		StorageConnections:  storageConnections,
	}
	if hadExisting {
		status.AllocatedVCPU = existing.Status.AllocatedVCPU
		status.AllocatedMemoryMB = existing.Status.AllocatedMemoryMB
	}

	out, err := s.hypervisors.Put(ctx, hypervisor, "", hypervisor, Hypervisor{Spec: spec, Status: status})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetSchedulable marks a Hypervisor schedulable or not, independent of its
// heartbeat-derived phase -- for planned maintenance, where the operator
// wants new VMs kept off a Hypervisor that's otherwise perfectly healthy
// (unlike NotReady, which only ever means "missed its last heartbeat").
func (s *Service) SetSchedulable(ctx context.Context, hypervisor string, schedulable bool) (*Hypervisor, error) {
	if err := s.updateHypervisor(ctx, hypervisor, func(h *Hypervisor) error {
		h.Spec.Schedulable = schedulable
		return nil
	}); err != nil {
		return nil, err
	}
	return s.GetHypervisor(ctx, hypervisor)
}

// SetRevoked revokes (or un-revokes) a Hypervisor id, blocking any future
// RegisterHypervisor call under that id -- for a decommissioned or
// compromised host (see ErrHypervisorRevoked). Revoking also forces
// Schedulable false in the same update (a revoked Hypervisor can't
// meaningfully stay schedulable); un-revoking does not restore it --
// that's left as a separate, explicit operator decision. This never
// touches already-flowing east-west traffic: with no per-hypervisor mTLS
// identity, there's nothing at that layer to revoke -- see
// docs/specs/hypervisor-bootstrap.md for the deliberate scope boundary.
func (s *Service) SetRevoked(ctx context.Context, hypervisor string, revoked bool) (*Hypervisor, error) {
	if err := s.updateHypervisor(ctx, hypervisor, func(h *Hypervisor) error {
		h.Spec.Revoked = revoked
		if revoked {
			h.Spec.Schedulable = false
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s.GetHypervisor(ctx, hypervisor)
}

func (s *Service) GetHypervisor(ctx context.Context, id string) (*Hypervisor, error) {
	out, err := s.hypervisors.Get(ctx, "", id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListHypervisors(ctx context.Context) ([]Hypervisor, error) {
	return s.hypervisors.List(ctx, "")
}

func (s *Service) WatchHypervisors(ctx context.Context, sinceRV int64) (<-chan HypervisorEvent, error) {
	return s.hypervisors.Watch(ctx, "", sinceRV, nil)
}

// Heartbeat records a compute-agent liveness signal, reviving the
// Hypervisor to Ready if the health sweep had marked it NotReady.
func (s *Service) Heartbeat(ctx context.Context, hypervisor string, at time.Time) error {
	return s.updateHypervisor(ctx, hypervisor, func(h *Hypervisor) error {
		h.Status.LastHeartbeatAt = at
		h.Status.Phase = HypervisorPhaseReady
		return nil
	})
}

// sweepHypervisorHealth marks every Hypervisor whose last heartbeat is
// older than heartbeatTimeout NotReady, excluding it from scheduling until
// it heartbeats again.
func (s *Service) sweepHypervisorHealth(ctx context.Context) {
	all, err := s.hypervisors.List(ctx, "")
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-heartbeatTimeout)
	for _, h := range all {
		if h.Status.Phase == HypervisorPhaseReady && h.Status.LastHeartbeatAt.Before(cutoff) {
			_ = s.updateHypervisor(ctx, h.Meta.ID, func(h *Hypervisor) error {
				h.Status.Phase = HypervisorPhaseNotReady
				return nil
			})
		}
	}
}

// runHealthSweep blocks, sweeping on healthSweepInterval until ctx is done.
func (s *Service) runHealthSweep(ctx context.Context) {
	ticker := time.NewTicker(healthSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepHypervisorHealth(ctx)
		}
	}
}

// scheduleVM picks a Hypervisor satisfying spec's hard constraints (see
// "フィルタ（ハード制約）"; PCI filters are deferred -- no PCI inventory
// exists yet), reserves capacity against it, and returns its id. Callers
// are responsible for then transitioning the VM to Scheduled and releasing
// the reservation (releaseHypervisorCapacity) if that fails.
//
// requiredZone (derived from spec.network_interfaces' Subnets by the
// caller -- see reconciler.go) restricts candidates to that zone; empty
// means no constraint (a VM with no network_interfaces yet, since
// compute-agent doesn't wire anything real regardless -- see
// docs/specs/network.md).
func (s *Service) scheduleVM(ctx context.Context, spec VirtualMachineSpec, requiredZone string) (string, error) {
	candidates, err := s.hypervisors.List(ctx, "")
	if err != nil {
		return "", err
	}
	driver := spec.DriverHint
	if driver == VmmDriverUnspecified {
		driver = VmmDriverFirecracker
	}
	filtered := filterSchedulable(candidates, driver, spec.VCPU, spec.MemoryMB, requiredZone)
	picked, err := s.scheduler.Pick(filtered)
	if err != nil {
		return "", err
	}

	if err := s.reserveHypervisorCapacity(ctx, picked.Meta.ID, spec.VCPU, spec.MemoryMB); err != nil {
		return "", err
	}
	return picked.Meta.ID, nil
}

func filterSchedulable(candidates []Hypervisor, driver VmmDriver, vcpu int32, memoryMB int64, requiredZone string) []Hypervisor {
	var out []Hypervisor
	for _, h := range candidates {
		if h.Status.Phase != HypervisorPhaseReady {
			continue
		}
		if !h.Spec.Schedulable {
			continue
		}
		if !hasDriver(h.Status.SupportedDrivers, driver) {
			continue
		}
		if h.Status.AllocatableVCPU-h.Status.AllocatedVCPU < vcpu {
			continue
		}
		if h.Status.AllocatableMemoryMB-h.Status.AllocatedMemoryMB < memoryMB {
			continue
		}
		if requiredZone != "" && h.Status.Zone != requiredZone {
			continue
		}
		out = append(out, h)
	}
	return out
}

func hasDriver(supported []string, driver VmmDriver) bool {
	for _, d := range supported {
		if VmmDriver(d) == driver {
			return true
		}
	}
	return false
}

// SchedulingStrategy picks among candidates that already satisfy every hard
// constraint. See docs/architecture.md "ピック（デフォルト戦略）": kept as an
// interface so a future strategy can be swapped in without touching the
// filters or the reservation transaction, but only one implementation
// exists so far -- YAGNI beyond that.
type SchedulingStrategy interface {
	Pick(candidates []Hypervisor) (*Hypervisor, error)
}

// MostAvailableFirst picks the candidate with the most spare vCPU
// (spreading a tenant's VMs across hypervisors rather than packing one
// first, without needing rack/failure-domain awareness).
type MostAvailableFirst struct{}

func (MostAvailableFirst) Pick(candidates []Hypervisor) (*Hypervisor, error) {
	var best *Hypervisor
	var bestAvail int32
	for i := range candidates {
		h := candidates[i]
		avail := h.Status.AllocatableVCPU - h.Status.AllocatedVCPU
		if best == nil || avail > bestAvail {
			best = &h
			bestAvail = avail
		}
	}
	if best == nil {
		return nil, ErrUnschedulable
	}
	return best, nil
}

// reserveHypervisorCapacity and releaseHypervisorCapacity implement
// "予約とレース対策": allocated_vcpu/memory_mb is adjusted via the generic
// Store's optimistic-concurrency Update, retrying on a concurrent
// modification rather than failing the whole schedule attempt.
//
// reserveHypervisorCapacity re-checks fit against the freshly-fetched h on
// every retry attempt, not just once against scheduleVM's earlier
// filterSchedulable snapshot -- without this, two concurrent scheduleVM
// calls that both pass filterSchedulable against the same slightly-stale
// List() (before either reservation has landed) could both proceed to
// reserve against the same Hypervisor: updateHypervisor's retry-on-conflict
// loop already guarantees the *arithmetic* is correct (no lost update --
// both deltas really do get added), but arithmetic correctness alone
// doesn't stop the sum from exceeding actual capacity. This is the same
// class of race OpenStack's nova-scheduler historically had before
// resource claims moved to the compute node's own ResourceTracker; here
// the fix is simpler since reservation already goes through a retry loop
// anyway -- it just needs to fail instead of blindly proceeding once it
// no longer fits. A caller that loses this race gets ErrUnschedulable,
// exactly as if no candidate had ever fit -- reconcile()'s PhasePending
// case already reports that as an Unschedulable condition and leaves the
// VM for runRetrySweep to retry (see [[kyuusha_stuck_phase_retry_sweep]]),
// which will re-List/re-filter/re-pick fresh next tick.
func (s *Service) reserveHypervisorCapacity(ctx context.Context, id string, vcpu int32, memoryMB int64) error {
	return s.updateHypervisor(ctx, id, func(h *Hypervisor) error {
		if h.Status.AllocatableVCPU-h.Status.AllocatedVCPU < vcpu ||
			h.Status.AllocatableMemoryMB-h.Status.AllocatedMemoryMB < memoryMB {
			return ErrUnschedulable
		}
		h.Status.AllocatedVCPU += vcpu
		h.Status.AllocatedMemoryMB += memoryMB
		return nil
	})
}

// resizeHypervisorCapacity adjusts a Hypervisor's Allocated{VCPU,MemoryMB}
// by (deltaVCPU, deltaMemoryMB) for a Resize of a VM already pinned to id --
// no candidate list, no filterSchedulable/Pick, just a capacity delta
// against the one Hypervisor this VM already sits on (see
// docs/specs/vm-scheduling.md's Resize section). Capacity is only checked
// when growing on a given axis (deltaVCPU/deltaMemoryMB > 0): a shrink
// (negative delta) always fits, since it only frees capacity. On success the
// delta is applied via the same += either direction relies on, so a shrink's
// negative delta correctly reduces the reservation.
func (s *Service) resizeHypervisorCapacity(ctx context.Context, id string, deltaVCPU int32, deltaMemoryMB int64) error {
	return s.updateHypervisor(ctx, id, func(h *Hypervisor) error {
		if deltaVCPU > 0 && h.Status.AllocatableVCPU-h.Status.AllocatedVCPU < deltaVCPU {
			return ErrHypervisorCapacityExceeded
		}
		if deltaMemoryMB > 0 && h.Status.AllocatableMemoryMB-h.Status.AllocatedMemoryMB < deltaMemoryMB {
			return ErrHypervisorCapacityExceeded
		}
		h.Status.AllocatedVCPU += deltaVCPU
		h.Status.AllocatedMemoryMB += deltaMemoryMB
		return nil
	})
}

func (s *Service) releaseHypervisorCapacity(ctx context.Context, id string, vcpu int32, memoryMB int64) {
	err := s.updateHypervisor(ctx, id, func(h *Hypervisor) error {
		h.Status.AllocatedVCPU -= vcpu
		h.Status.AllocatedMemoryMB -= memoryMB
		return nil
	})
	if err != nil && !errors.Is(err, ErrHypervisorNotFound) {
		// Best-effort: the Hypervisor is gone or unreachable via retries: no
		// capacity to release against, or a bug worth surfacing in logs by
		// the caller. Either way there's no further compensating action to
		// take here (Saga-style: log and move on, per docs/architecture.md's
		// rollback philosophy).
		_ = err
	}
}

// updateHypervisor is Get-mutate-Update with a bounded retry on
// resource_version conflicts, the shape every small Hypervisor status
// mutation here needs (Heartbeat, health sweep, capacity reserve/release).
// mutate sees a freshly-fetched h on every attempt (not just the first),
// so it can validate against current state -- not just prior state -- each
// time it's retried; an error from mutate itself aborts immediately
// (returned as-is, not retried), distinct from an ErrHypervisorConflict
// from Update, which retries with a fresh Get as usual.
func (s *Service) updateHypervisor(ctx context.Context, id string, mutate func(*Hypervisor) error) error {
	for range 20 {
		h, err := s.hypervisors.Get(ctx, "", id)
		if err != nil {
			return err
		}
		if err := mutate(&h); err != nil {
			return err
		}
		if _, err := s.hypervisors.Update(ctx, h); err != nil {
			if errors.Is(err, ErrHypervisorConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return ErrHypervisorConflict
}
