package compute

import (
	"context"
	"errors"
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

var (
	ErrHypervisorNotFound      = errors.New("hypervisor: not found")
	ErrHypervisorConflict      = errors.New("hypervisor: resource_version conflict")
	ErrHypervisorHistoryPruned = errors.New("hypervisor: watch resume point too old, relist required")
	ErrUnschedulable           = errors.New("vm: no hypervisor satisfies scheduling constraints")
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
// The doc's real design verifies a zone-scoped bootstrap token and issues an
// mTLS client cert here, trusting the token's zone over the agent's own
// claim. Neither exists yet (see hack/devkeys' JWT signer for the same kind
// of "real thing is later" gap on the authn side): this accepts the agent's
// self-reported zone/capacity directly, unauthenticated. Tracked in
// docs/open-questions.md.
func (s *Service) RegisterHypervisor(ctx context.Context, hypervisor, zone string, allocatableVCPU int32, allocatableMemoryMB int64, supportedDrivers []string) (*Hypervisor, error) {
	existing, err := s.hypervisors.Get(ctx, "", hypervisor)
	hadExisting := err == nil

	status := HypervisorStatus{
		Phase:               HypervisorPhaseReady,
		Zone:                zone,
		LastHeartbeatAt:     time.Now(),
		AllocatableVCPU:     allocatableVCPU,
		AllocatableMemoryMB: allocatableMemoryMB,
		SupportedDrivers:    supportedDrivers,
	}
	if hadExisting {
		status.AllocatedVCPU = existing.Status.AllocatedVCPU
		status.AllocatedMemoryMB = existing.Status.AllocatedMemoryMB
	}

	out := s.hypervisors.Put(ctx, hypervisor, "", hypervisor, Hypervisor{Status: status})
	return &out, nil
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
	return s.hypervisors.Watch(ctx, "", sinceRV)
}

// Heartbeat records a compute-agent liveness signal, reviving the
// Hypervisor to Ready if the health sweep had marked it NotReady.
func (s *Service) Heartbeat(ctx context.Context, hypervisor string, at time.Time) error {
	return s.updateHypervisor(ctx, hypervisor, func(h *Hypervisor) {
		h.Status.LastHeartbeatAt = at
		h.Status.Phase = HypervisorPhaseReady
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
			_ = s.updateHypervisor(ctx, h.Meta.ID, func(h *Hypervisor) {
				h.Status.Phase = HypervisorPhaseNotReady
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
// "フィルタ（ハード制約）"; zone and PCI filters are deferred -- no
// network/PCI inventory exists yet), reserves capacity against it, and
// returns its id. Callers are responsible for then transitioning the VM to
// Scheduled and releasing the reservation (releaseHypervisorCapacity) if
// that fails.
func (s *Service) scheduleVM(ctx context.Context, spec VirtualMachineSpec) (string, error) {
	candidates, err := s.hypervisors.List(ctx, "")
	if err != nil {
		return "", err
	}
	driver := spec.DriverHint
	if driver == VmmDriverUnspecified {
		driver = VmmDriverFirecracker
	}
	filtered := filterSchedulable(candidates, driver, spec.VCPU, spec.MemoryMB)
	picked, err := s.scheduler.Pick(filtered)
	if err != nil {
		return "", err
	}

	if err := s.reserveHypervisorCapacity(ctx, picked.Meta.ID, spec.VCPU, spec.MemoryMB); err != nil {
		return "", err
	}
	return picked.Meta.ID, nil
}

func filterSchedulable(candidates []Hypervisor, driver VmmDriver, vcpu int32, memoryMB int64) []Hypervisor {
	var out []Hypervisor
	for _, h := range candidates {
		if h.Status.Phase != HypervisorPhaseReady {
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
func (s *Service) reserveHypervisorCapacity(ctx context.Context, id string, vcpu int32, memoryMB int64) error {
	return s.updateHypervisor(ctx, id, func(h *Hypervisor) {
		h.Status.AllocatedVCPU += vcpu
		h.Status.AllocatedMemoryMB += memoryMB
	})
}

func (s *Service) releaseHypervisorCapacity(ctx context.Context, id string, vcpu int32, memoryMB int64) {
	err := s.updateHypervisor(ctx, id, func(h *Hypervisor) {
		h.Status.AllocatedVCPU -= vcpu
		h.Status.AllocatedMemoryMB -= memoryMB
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
func (s *Service) updateHypervisor(ctx context.Context, id string, mutate func(*Hypervisor)) error {
	for range 20 {
		h, err := s.hypervisors.Get(ctx, "", id)
		if err != nil {
			return err
		}
		mutate(&h)
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
