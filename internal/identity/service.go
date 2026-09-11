package identity

import (
	"context"
	"errors"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	ErrNotFound      = errors.New("tenant: not found")
	ErrConflict      = errors.New("tenant: resource_version conflict")
	ErrHistoryPruned = errors.New("tenant: watch resume point too old, relist required")
)

// EventType and Event are re-exported from the generic resource.Store so
// callers keep writing identity.Event / identity.EventAdded etc.
type EventType = resource.EventType
type Event = resource.Event[Tenant]

const (
	EventAdded    = resource.EventAdded
	EventModified = resource.EventModified
	EventDeleted  = resource.EventDeleted
	EventBookmark = resource.EventBookmark
)

// Service implements the TenantService CRUD+Watch surface against an
// in-memory resource.Store. Unlike compute's VirtualMachine, a Tenant has
// nothing to provision -- creating the record is the entire action -- so
// there is no scheduler/reconciler here: Create sets Phase Active
// synchronously.
type Service struct {
	store *resource.Store[Tenant, *Tenant]
}

func NewService(etcdClient *clientv3.Client) *Service {
	return &Service{
		store: resource.NewStore[Tenant, *Tenant](etcdClient, "tenant", resource.StoreErrors{
			NotFound:      ErrNotFound,
			Conflict:      ErrConflict,
			HistoryPruned: ErrHistoryPruned,
		}),
	}
}

// Create is idempotent when name is set: a second Create with the same
// name returns the existing Tenant rather than minting a new one. The
// minted Meta.ID becomes the Tenant's own tenant_id (self-referential),
// which is what other services' tenant_id fields, and future JWT
// tenant_id claims, are expected to reference.
func (s *Service) Create(ctx context.Context, name string, spec TenantSpec) (*Tenant, error) {
	out, err := s.store.CreateSelfReferential(ctx, name, Tenant{
		Spec:   spec,
		Status: TenantStatus{Phase: PhaseActive},
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Get(ctx context.Context, tenantID string) (*Tenant, error) {
	out, err := s.store.Get(ctx, tenantID, tenantID)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every Tenant matching tenantID, or every Tenant when
// tenantID is empty. Since a Tenant's own id and tenant_id are the same
// value, a non-empty tenantID returns at most the caller's own Tenant;
// listing more than that is effectively admin-only (see internal/authz).
func (s *Service) List(ctx context.Context, tenantID string) ([]Tenant, error) {
	return s.store.List(ctx, tenantID)
}

// Update requires tenant.Meta.ResourceVersion to match the stored value
// (optimistic concurrency); mismatches return ErrConflict.
func (s *Service) Update(ctx context.Context, tenant *Tenant) (*Tenant, error) {
	out, err := s.store.Update(ctx, *tenant)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Delete(ctx context.Context, tenantID string) error {
	return s.store.Delete(ctx, tenantID, tenantID)
}

// Watch replays history newer than sinceRV (0 for "from the start") and
// then streams live events, scoped to tenantID (empty = all tenants,
// admin-only in practice). The returned channel is closed when ctx is done.
func (s *Service) Watch(ctx context.Context, tenantID string, sinceRV int64) (<-chan Event, error) {
	return s.store.Watch(ctx, tenantID, sinceRV, nil)
}
