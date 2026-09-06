package compute

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/image/v1"
	networkv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

var (
	ErrNotFound      = errors.New("vm: not found")
	ErrConflict      = errors.New("vm: resource_version conflict")
	ErrValidation    = errors.New("vm: validation failed")
	ErrHistoryPruned = errors.New("vm: watch resume point too old, relist required")
	ErrQuotaExceeded = errors.New("vm: tenant quota exceeded")
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
// docs/architecture.md against an in-memory resource.Store, plus Create-time
// Quota enforcement against identity (see "Quota設計"): identity holds the
// limit values only, compute tracks its own tenant_usage and enforces it.
//
// usageMu serializes the whole "look up idempotency, fetch quota, check,
// charge, create" sequence globally, across all tenants -- simple and
// correct (no lost updates, no double-charging an idempotent retry), but a
// real scalability bottleneck at higher creates/sec than this system's
// target scale implies. Sharding it per tenant is a reasonable follow-up if
// that ever matters.
type Service struct {
	store          *resource.Store[VirtualMachine, *VirtualMachine]
	hypervisors    *resource.Store[Hypervisor, *Hypervisor]
	scheduler      SchedulingStrategy
	identityClient identityv1.TenantServiceClient
	imageClient    imagev1.ImageServiceClient
	subnetClient   networkv1.SubnetServiceClient
	netifClient    networkv1.NetworkInterfaceServiceClient
	quota          *quotaChecker

	usageMu sync.Mutex
	usage   map[string]tenantUsage
}

func NewService(ctx context.Context, identityClient identityv1.TenantServiceClient, imageClient imagev1.ImageServiceClient, subnetClient networkv1.SubnetServiceClient, netifClient networkv1.NetworkInterfaceServiceClient) (*Service, error) {
	quota, err := newQuotaChecker(ctx)
	if err != nil {
		return nil, err
	}
	return &Service{
		store: resource.NewStore[VirtualMachine, *VirtualMachine]("vm", resource.StoreErrors{
			NotFound:      ErrNotFound,
			Conflict:      ErrConflict,
			HistoryPruned: ErrHistoryPruned,
		}),
		hypervisors: resource.NewStore[Hypervisor, *Hypervisor]("hypervisor", resource.StoreErrors{
			NotFound:      ErrHypervisorNotFound,
			Conflict:      ErrHypervisorConflict,
			HistoryPruned: ErrHypervisorHistoryPruned,
		}),
		scheduler:      MostAvailableFirst{},
		identityClient: identityClient,
		imageClient:    imageClient,
		subnetClient:   subnetClient,
		netifClient:    netifClient,
		quota:          quota,
		usage:          make(map[string]tenantUsage),
	}, nil
}

// Create is idempotent when Name is set: a second Create with the same
// (tenantID, name) returns the existing VirtualMachine rather than erroring
// or re-charging quota. A genuinely new Create synchronously checks
// tenantID's Quota (fetched from identity) against compute's own local
// tenant_usage and rejects with ErrQuotaExceeded if it would be exceeded,
// per "Quota設計": a doomed VirtualMachine is never created just to be
// marked Error afterwards.
func (s *Service) Create(ctx context.Context, tenantID, name string, spec VirtualMachineSpec) (*VirtualMachine, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if spec.ImageID == "" {
		return nil, fmt.Errorf("%w: spec.image_id is required", ErrValidation)
	}
	if spec.RecoveryPolicy == RecoveryPolicyUnspecified {
		return nil, fmt.Errorf("%w: spec.recovery_policy must be set", ErrValidation)
	}
	if spec.DriverHint == VmmDriverUnspecified {
		spec.DriverHint = VmmDriverFirecracker
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if existing, ok := s.store.LookupByName(tenantID, name); ok {
		return &existing, nil
	}

	if err := validateImage(ctx, s.imageClient, tenantID, spec.ImageID, spec.DriverHint); err != nil {
		return nil, err
	}
	if _, err := validateNetworkInterfaces(ctx, s.subnetClient, tenantID, spec.NetworkInterfaces); err != nil {
		return nil, err
	}

	limit, err := lookupQuota(ctx, s.identityClient, tenantID)
	if err != nil {
		return nil, err
	}
	usage := s.usage[tenantID]
	allowed, err := s.quota.allow(ctx, usage, spec.VCPU, spec.MemoryMB, limit)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("%w: tenant %q", ErrQuotaExceeded, tenantID)
	}

	out, err := s.store.Create(ctx, tenantID, name, VirtualMachine{
		Spec:   spec,
		Status: VirtualMachineStatus{Phase: PhasePending},
	})
	if err != nil {
		return nil, err
	}

	usage.VCPU += spec.VCPU
	usage.MemoryMB += spec.MemoryMB
	usage.VMCount++
	s.usage[tenantID] = usage

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
// (optimistic concurrency); mismatches return ErrConflict. Spec fields
// (vcpu/memory_mb) aren't expected to change via Update -- there's no
// resize feature -- so this doesn't touch tenant_usage.
func (s *Service) Update(ctx context.Context, machine *VirtualMachine) (*VirtualMachine, error) {
	out, err := s.store.Update(ctx, *machine)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes the VM and, per "Quota設計", decrements tenant_usage in
// the same critical section.
//
// If the VM carries Finalizers (see docs/architecture.md "Finalizer"),
// store.Delete doesn't actually remove it -- it lingers with DeletedAt set
// until every Finalizer entry is cleared via Update. This method still
// decrements tenant_usage immediately regardless: quota accounting is
// tied to "Delete was requested", not to the object's eventual real
// removal, the same "don't do live SUM aggregation" simplification
// Quota設計 already accepts elsewhere. The one-time cost is that a tenant
// could transiently look like it has more quota headroom than a strictly
// literal reading of "what still physically exists" would allow, for as
// long as a Finalizer blocks the real removal -- accepted as harmless
// since it's self-inflicted (the same tenant's own VM) and not a cross-
// tenant safety issue. reconcile()'s physical teardown (releasing
// Hypervisor capacity, telling compute-agent to stop the process) is
// unaffected either way: it's driven by the Deleted event, which now
// correctly only fires once Finalizers actually clear.
func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	vm, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	// A repeated Delete call while Finalizers are still pending finds the
	// object still here (DeletedAt already set by the first call): don't
	// decrement tenant_usage a second time for the same VM.
	alreadyPendingDeletion := vm.Meta.DeletedAt != nil

	if len(vm.Meta.Finalizers) > 0 && vm.Status.Phase != PhaseDeleting {
		vm.Status.Phase = PhaseDeleting
		updated, err := s.store.Update(ctx, vm)
		if err != nil {
			return err
		}
		vm = updated
	}

	if err := s.store.Delete(ctx, tenantID, id); err != nil {
		return err
	}
	if alreadyPendingDeletion {
		return nil
	}

	usage := s.usage[tenantID]
	usage.VCPU -= vm.Spec.VCPU
	usage.MemoryMB -= vm.Spec.MemoryMB
	usage.VMCount--
	s.usage[tenantID] = usage

	return nil
}

// Watch replays history newer than sinceRV (0 for "from the start") and then
// streams live events, both scoped to tenantID. An empty tenantID watches
// across all tenants, for internal use by the reconciler; external callers
// must always pass their own tenant_id. The returned channel is closed when
// ctx is done.
func (s *Service) Watch(ctx context.Context, tenantID string, sinceRV int64) (<-chan Event, error) {
	return s.store.Watch(ctx, tenantID, sinceRV)
}
