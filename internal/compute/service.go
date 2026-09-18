package compute

import (
	"context"
	"errors"
	"fmt"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/resource"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

var (
	ErrNotFound      = errors.New("vm: not found")
	ErrConflict      = errors.New("vm: resource_version conflict")
	ErrValidation    = errors.New("vm: validation failed")
	ErrHistoryPruned = errors.New("vm: watch resume point too old, relist required")
	ErrQuotaExceeded = errors.New("vm: tenant quota exceeded")
	ErrInvalidPhase  = errors.New("vm: not in a phase this operation allows")
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
	store                  *resource.Store[VirtualMachine, *VirtualMachine]
	hypervisors            *resource.Store[Hypervisor, *Hypervisor]
	scheduler              SchedulingStrategy
	identityClient         identityv1.TenantServiceClient
	imageClient            imagev1.ImageServiceClient
	subnetClient           networkv1.SubnetServiceClient
	netifClient            networkv1.NetworkInterfaceServiceClient
	volumeClient           blockstoragev1.VolumeServiceClient
	volumeAttachmentClient blockstoragev1.VolumeAttachmentServiceClient
	quota                  *quotaChecker

	usageMu sync.Mutex
	usage   map[string]tenantUsage
}

// NewService constructs a Service and synchronously rebuilds its tenant
// quota usage (see rebuildUsage) from etcd before returning -- same
// reasoning as network.NewService's/blockstorage.NewService's identical
// rebuild calls: callers must not start serving Create/Delete requests
// until this returns.
func NewService(ctx context.Context, etcdClient *clientv3.Client, identityClient identityv1.TenantServiceClient, imageClient imagev1.ImageServiceClient, subnetClient networkv1.SubnetServiceClient, netifClient networkv1.NetworkInterfaceServiceClient, volumeClient blockstoragev1.VolumeServiceClient, volumeAttachmentClient blockstoragev1.VolumeAttachmentServiceClient) (*Service, error) {
	quota, err := newQuotaChecker(ctx)
	if err != nil {
		return nil, err
	}
	svc := &Service{
		store: resource.NewStore[VirtualMachine, *VirtualMachine](etcdClient, "vm", resource.StoreErrors{
			NotFound:      ErrNotFound,
			Conflict:      ErrConflict,
			HistoryPruned: ErrHistoryPruned,
		}),
		hypervisors: resource.NewStore[Hypervisor, *Hypervisor](etcdClient, "hypervisor", resource.StoreErrors{
			NotFound:      ErrHypervisorNotFound,
			Conflict:      ErrHypervisorConflict,
			HistoryPruned: ErrHypervisorHistoryPruned,
		}),
		scheduler:              MostAvailableFirst{},
		identityClient:         identityClient,
		imageClient:            imageClient,
		subnetClient:           subnetClient,
		netifClient:            netifClient,
		volumeClient:           volumeClient,
		volumeAttachmentClient: volumeAttachmentClient,
		quota:                  quota,
		usage:                  make(map[string]tenantUsage),
	}
	if err := svc.rebuildUsage(ctx); err != nil {
		return nil, err
	}
	return svc, nil
}

// rebuildUsage restores usage's in-memory per-tenant quota accounting from
// every existing VirtualMachine in etcd. Found 2026-09-13 (same bug class
// as network.Service.rebuildPools/blockstorage.Service.rebuildUsage, and
// present here even before this session's compute-reconciler split --
// that split just made it newly relevant, since the whole point of
// running compute's gRPC API as multiple replicas requires this to
// actually be safe): usage is purely in-memory, populated only by
// Create/Delete calls made within this process's own lifetime, so without
// this, EVERY restart forgets every tenant's real usage and lets Create
// approve requests a live tenant_usage would have rejected -- and,
// separately, two API replicas would each maintain their own independent
// usage map that drifts the moment either one processes a Create/Delete,
// letting a tenant split requests across replicas to bypass quota
// entirely. Excludes a VM with Meta.DeletedAt already set: Delete's first
// call already decremented usage for it (see Delete's own doc comment on
// why that happens before the object is actually gone, not after).
func (s *Service) rebuildUsage(ctx context.Context) error {
	vms, err := s.store.List(ctx, "")
	if err != nil {
		return fmt.Errorf("compute: rebuild usage: list virtual machines: %w", err)
	}
	usage := make(map[string]tenantUsage)
	for _, vm := range vms {
		if vm.Meta.DeletedAt != nil {
			continue
		}
		u := usage[vm.Meta.TenantID]
		u.VCPU += vm.Spec.VCPU
		u.MemoryMB += vm.Spec.MemoryMB
		u.VMCount++
		usage[vm.Meta.TenantID] = u
	}
	s.usage = usage
	return nil
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
	if spec.DriverHint == VmmDriverUnspecified {
		spec.DriverHint = VmmDriverFirecracker
	}
	if err := validateVCPUForDriver(spec.VCPU, spec.DriverHint); err != nil {
		return nil, err
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if existing, ok := s.store.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}

	if err := validateImage(ctx, s.imageClient, tenantID, spec.ImageID, spec.DriverHint); err != nil {
		return nil, err
	}
	if _, err := validateNetworkInterfaces(ctx, s.subnetClient, tenantID, spec.NetworkInterfaces); err != nil {
		return nil, err
	}
	if err := validateVolumes(ctx, s.volumeClient, tenantID, spec.Volumes); err != nil {
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
// (optimistic concurrency); mismatches return ErrConflict. Spec.VCPU/
// Spec.MemoryMB aren't expected to change via Update -- that's Resize's job
// (Stopped-only, with its own quota/Hypervisor-capacity accounting) -- so
// this doesn't touch tenant_usage.
func (s *Service) Update(ctx context.Context, machine *VirtualMachine) (*VirtualMachine, error) {
	current, err := s.store.Get(ctx, machine.Meta.TenantID, machine.Meta.ID)
	if err != nil {
		return nil, err
	}
	finalizers, err := checkFinalizerMutation(ctx, current.Meta.Finalizers, machine.Meta.Finalizers)
	if err != nil {
		return nil, err
	}
	machine.Meta.Finalizers = finalizers

	out, err := s.store.Update(ctx, *machine)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// checkFinalizerMutation reconciles the Finalizers an Update caller
// requested against what's actually stored, the same fetch-then-merge
// shape as every other server-stamped field: a client can never set or
// change an entry's AddedBy. A newly added entry (present in requested,
// absent from stored) gets AddedBy stamped from the caller's real,
// propagated identity (internal/authn/propagate.go) -- any client-supplied
// AddedBy is discarded. Removing an entry (present in stored, absent from
// requested) requires the caller's sub to match that entry's AddedBy, or
// the admin role, UNLESS the stored AddedBy is itself empty (an entry added
// before this identity plumbing existed, or by a caller that never had a
// propagated identity -- e.g. an internal caller that bypasses api-gateway):
// an unowned entry stays removable by anyone, exactly like before this
// check existed. Any other mismatch is rejected with ErrValidation and the
// finalizer is left in place, unchanged. See docs/architecture.md
// "Finalizer" for why this authorization exists at all -- clearing someone
// else's finalizer would let a tenant remove a hold an external controller
// placed to protect a resource it doesn't own.
func checkFinalizerMutation(ctx context.Context, stored, requested []resource.Finalizer) ([]resource.Finalizer, error) {
	storedByName := make(map[string]resource.Finalizer, len(stored))
	for _, f := range stored {
		storedByName[f.Name] = f
	}
	requestedNames := make(map[string]struct{}, len(requested))

	callerSub, _ := authn.CallerSubFromContext(ctx)
	isAdmin := authn.CallerIsAdminFromContext(ctx)

	out := make([]resource.Finalizer, len(requested))
	for i, f := range requested {
		requestedNames[f.Name] = struct{}{}
		if existing, ok := storedByName[f.Name]; ok {
			out[i] = existing
			continue
		}
		out[i] = resource.Finalizer{Name: f.Name, AddedBy: callerSub}
	}

	for _, f := range stored {
		if _, stillPresent := requestedNames[f.Name]; stillPresent {
			continue
		}
		if f.AddedBy != "" && !isAdmin && callerSub != f.AddedBy {
			return nil, fmt.Errorf("%w: finalizer %q can only be removed by %q or an admin", ErrValidation, f.Name, f.AddedBy)
		}
	}

	return out, nil
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

// Stop moves a Running VM to Stopping: reconcile() (reconciler.go) picks up
// the transition and tells compute-agent to tear down the real VMM process,
// reporting back on EvtSubjectStopResult once it actually has (see
// handleStopResult) -- Stop itself does not wait for that here, matching
// this system's general "Create -> Watch until done" async pattern (see
// docs/architecture.md) rather than blocking the RPC on it.
//
// force is stashed on Status.StopForce only to cross into reconcile()'s
// Watch-driven loop (see virtualmachine.go's doc comment on that field) --
// it is not part of the VM's durable state.
//
// Unlike Delete, this does NOT touch tenant_usage or release the
// Hypervisor's capacity reservation: the whole point of Stop (as opposed to
// Delete) is that the VM keeps its assignment and its root disk, ready for a
// later Start -- see docs/architecture.md's VM lifecycle section.
func (s *Service) Stop(ctx context.Context, tenantID, id string, force bool) (*VirtualMachine, error) {
	vm, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if vm.Status.Phase != PhaseRunning {
		return nil, fmt.Errorf("%w: vm must be Running to Stop (phase=%s)", ErrInvalidPhase, vm.Status.Phase)
	}
	vm.Status.Phase = PhaseStopping
	vm.Status.StopForce = force
	out, err := s.store.Update(ctx, vm)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Start moves a Stopped VM to Starting: reconcile() (reconciler.go) picks up
// the transition and drives it straight through Provisioning exactly like a
// freshly-Scheduled VM (see provisionAndPublish), reattaching its existing
// NetworkInterfaces/VolumeAttachments and telling compute-agent to boot it
// again -- each VMM driver's Boot detects and reuses the already-placed root
// disk from before Stop rather than recopying it from the Image (see
// internal/compute-agent/fcvmm and .../chvmm).
func (s *Service) Start(ctx context.Context, tenantID, id string) (*VirtualMachine, error) {
	vm, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if vm.Status.Phase != PhaseStopped {
		return nil, fmt.Errorf("%w: vm must be Stopped to Start (phase=%s)", ErrInvalidPhase, vm.Status.Phase)
	}
	vm.Status.Phase = PhaseStarting
	out, err := s.store.Update(ctx, vm)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Resize changes a Stopped VM's vcpu/memory_mb in place (cold resize only --
// see docs/specs/virtual-machine.md). No live/hot resize exists: cloud-
// hypervisor's API socket is deliberately unused (docs/specs/cloud-
// hypervisor-boot.md), so the only way to change a booted VM's size is to
// stop it, change Spec here, and Start it again -- Start's reconcile() path
// (provisionAndPublish) already rebuilds its CreateCommand straight from
// Spec every time, so no VMM-driver/reconciler changes are needed for the
// new size to take effect.
//
// Like Create/Delete, this holds usageMu for the whole check-then-commit
// sequence since it adjusts tenant_usage; VMCount is left untouched (a
// resize never creates or removes a VM). Unlike Create, the Hypervisor this
// VM is already pinned to is never re-picked -- only a capacity delta is
// applied against it (resizeHypervisorCapacity), and if the new size
// doesn't fit there, the resize is rejected outright rather than migrating
// to a different Hypervisor (see docs/specs/vm-scheduling.md).
func (s *Service) Resize(ctx context.Context, tenantID, id string, vcpu int32, memoryMB int64) (*VirtualMachine, error) {
	if vcpu <= 0 {
		return nil, fmt.Errorf("%w: vcpu must be positive", ErrValidation)
	}
	if memoryMB <= 0 {
		return nil, fmt.Errorf("%w: memory_mb must be positive", ErrValidation)
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	vm, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if vm.Status.Phase != PhaseStopped {
		return nil, fmt.Errorf("%w: vm must be Stopped to Resize (phase=%s)", ErrInvalidPhase, vm.Status.Phase)
	}
	if err := validateVCPUForDriver(vcpu, vm.Spec.DriverHint); err != nil {
		return nil, err
	}

	if vcpu == vm.Spec.VCPU && memoryMB == vm.Spec.MemoryMB {
		return &vm, nil
	}

	deltaVCPU := vcpu - vm.Spec.VCPU
	deltaMemoryMB := memoryMB - vm.Spec.MemoryMB

	// A pure shrink (both deltas <= 0) can never violate quota -- it only
	// frees headroom -- so identity isn't consulted at all in that case.
	if deltaVCPU > 0 || deltaMemoryMB > 0 {
		limit, err := lookupQuota(ctx, s.identityClient, tenantID)
		if err != nil {
			return nil, err
		}
		usage := s.usage[tenantID]
		allowed, err := s.quota.allowResize(ctx, usage, deltaVCPU, deltaMemoryMB, vcpu, memoryMB, limit)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, fmt.Errorf("%w: tenant %q", ErrQuotaExceeded, tenantID)
		}
	}

	if err := s.resizeHypervisorCapacity(ctx, vm.Status.Hypervisor, deltaVCPU, deltaMemoryMB); err != nil {
		return nil, err
	}

	vm.Spec.VCPU = vcpu
	vm.Spec.MemoryMB = memoryMB
	out, err := s.store.Update(ctx, vm)
	if err != nil {
		// Roll back the capacity delta already applied above -- the same
		// delta values undo it in either direction (a grow's rollback
		// subtracts what was added, a shrink's rollback adds back what was
		// released), same Saga-style compensating-action shape
		// releaseHypervisorCapacity/reconcile() already use elsewhere.
		s.releaseHypervisorCapacity(ctx, vm.Status.Hypervisor, deltaVCPU, deltaMemoryMB)
		return nil, err
	}

	usage := s.usage[tenantID]
	usage.VCPU += deltaVCPU
	usage.MemoryMB += deltaMemoryMB
	s.usage[tenantID] = usage

	return &out, nil
}

// Watch replays history newer than sinceRV (0 for "from the start") and then
// streams live events, both scoped to tenantID. An empty tenantID watches
// across all tenants, for internal use by the reconciler; external callers
// must always pass their own tenant_id. An empty finalizerName streams
// every VM as before; a non-empty one restricts the replay and live stream
// to VMs whose meta.finalizers currently contains an entry with that name
// -- see docs/architecture.md "Finalizer" 's discussion of external
// controllers watching at scale: a controller that only cares about VMs it
// has itself placed a finalizer on should pass its own name here instead of
// watching (and filtering client-side) every VM in the tenant. The
// returned channel is closed when ctx is done.
func (s *Service) Watch(ctx context.Context, tenantID string, sinceRV int64, finalizerName string) (<-chan Event, error) {
	var matches func(VirtualMachine) bool
	if finalizerName != "" {
		matches = func(vm VirtualMachine) bool {
			for _, f := range vm.Meta.Finalizers {
				if f.Name == finalizerName {
					return true
				}
			}
			return false
		}
	}
	return s.store.Watch(ctx, tenantID, sinceRV, matches)
}
