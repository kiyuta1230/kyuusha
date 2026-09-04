package compute

import (
	"context"
	"errors"
	"fmt"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

var (
	ErrNotFound      = errors.New("vm: not found")
	ErrConflict      = errors.New("vm: resource_version conflict")
	ErrValidation    = errors.New("vm: validation failed")
	ErrHistoryPruned = errors.New("vm: watch resume point too old, relist required")
)

// EventType and Event are re-exported from the generic resource.Store so
// callers keep writing compute.Event / compute.EventAdded etc.
type EventType = resource.EventType
type Event = resource.Event[VirtualMachine]

const (
	EventAdded    = resource.EventAdded
	EventModified = resource.EventModified
	EventDeleted  = resource.EventDeleted
	EventBookmark = resource.EventBookmark
)

// Service implements the VirtualMachineService CRUD+Watch surface from
// docs/architecture.md against an in-memory resource.Store. This is the
// first, scheduler/NATS-free slice: it validates the shape of the
// declarative resource model (ObjectMeta, resource_version, Watch) end to
// end.
type Service struct {
	store *resource.Store[VirtualMachine, *VirtualMachine]
}

func NewService() *Service {
	return &Service{
		store: resource.NewStore[VirtualMachine, *VirtualMachine]("vm", resource.StoreErrors{
			NotFound:      ErrNotFound,
			Conflict:      ErrConflict,
			HistoryPruned: ErrHistoryPruned,
		}),
	}
}

// Create is idempotent when Name is set: a second Create with the same
// (tenantID, name) returns the existing VirtualMachine rather than erroring.
func (s *Service) Create(ctx context.Context, tenantID, name string, spec VirtualMachineSpec) (*VirtualMachine, error) {
	if spec.RecoveryPolicy == RecoveryPolicyUnspecified {
		return nil, fmt.Errorf("%w: spec.recovery_policy must be set", ErrValidation)
	}
	if spec.DriverHint == VmmDriverUnspecified {
		spec.DriverHint = VmmDriverFirecracker
	}

	out, err := s.store.Create(ctx, tenantID, name, VirtualMachine{
		Spec:   spec,
		Status: VirtualMachineStatus{Phase: PhasePending},
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (*VirtualMachine, error) {
	out, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every VM for tenantID, or every VM across all tenants when
// tenantID is empty (internal use only; external callers must always pass
// their own tenant_id).
func (s *Service) List(ctx context.Context, tenantID string) ([]VirtualMachine, error) {
	return s.store.List(ctx, tenantID)
}

// Update requires machine.Meta.ResourceVersion to match the stored value
// (optimistic concurrency); mismatches return ErrConflict.
func (s *Service) Update(ctx context.Context, machine *VirtualMachine) (*VirtualMachine, error) {
	out, err := s.store.Update(ctx, *machine)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.store.Delete(ctx, tenantID, id)
}

// Watch replays history newer than sinceRV (0 for "from the start") and then
// streams live events, both scoped to tenantID. An empty tenantID watches
// across all tenants, for internal use by the reconciler; external callers
// must always pass their own tenant_id. The returned channel is closed when
// ctx is done.
func (s *Service) Watch(ctx context.Context, tenantID string, sinceRV int64) (<-chan Event, error) {
	return s.store.Watch(ctx, tenantID, sinceRV)
}
