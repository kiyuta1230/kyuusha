package resource

import (
	"context"
	"sync"
	"time"
)

type EventType string

const (
	EventAdded    EventType = "ADDED"
	EventModified EventType = "MODIFIED"
	EventDeleted  EventType = "DELETED"
	EventBookmark EventType = "BOOKMARK"
)

type Event[T any] struct {
	Type            EventType
	Object          T
	ResourceVersion int64
}

// StoreErrors lets each Store instance return its own sentinel error values
// (e.g. compute wants "vm: not found", identity wants "tenant: not found")
// while sharing one Create/Get/List/Update/Delete/Watch implementation.
// Validation errors are deliberately not included here: they're
// domain-specific (e.g. "recovery_policy must be set") and stay owned by
// the calling service, checked before Store.Create is invoked.
type StoreErrors struct {
	NotFound      error
	Conflict      error
	HistoryPruned error
}

// Store is the generic Create/Get/List/Update/Delete/Watch engine shared by
// every kyuusha resource type: ObjectMeta bookkeeping (ID minting,
// resource_version, idempotent-by-name Create), a bounded event history,
// and Watch (replay-then-stream, with periodic Bookmarks so a client can
// tell "caught up" from "nothing happened yet"). T is the resource value
// type (e.g. compute.VirtualMachine); PT is its pointer type, which must
// satisfy Meta -- see that type's doc comment for the embedding pattern.
type Store[T any, PT interface {
	*T
	Meta
}] struct {
	mu             sync.RWMutex
	byID           map[string]T
	byTenantName   map[string]string // "<tenantID>/<name>" -> id, for idempotent Create
	history        []Event[T]
	historyLimit   int
	nextRV         int64
	watchers       map[chan Event[T]]struct{}
	bookmarkPeriod time.Duration
	idPrefix       string
	errs           StoreErrors
}

func NewStore[T any, PT interface {
	*T
	Meta
}](idPrefix string, errs StoreErrors) *Store[T, PT] {
	return &Store[T, PT]{
		byID:           make(map[string]T),
		byTenantName:   make(map[string]string),
		historyLimit:   1000,
		watchers:       make(map[chan Event[T]]struct{}),
		bookmarkPeriod: 30 * time.Second,
		idPrefix:       idPrefix,
		errs:           errs,
	}
}

// Create is idempotent when name is set: a second Create for the same
// (tenantID, name) returns the existing object rather than minting a new
// one. obj should already carry its domain Spec/Status; Create fills in
// Meta (ID, TenantID, Name, CreatedAt, ResourceVersion).
func (s *Store[T, PT]) Create(ctx context.Context, tenantID, name string, obj T) (T, error) {
	return s.create(NewID(s.idPrefix), tenantID, tenantID, name, obj)
}

// CreateSelfReferential is Create for resources whose own tenant_id equals
// their id (e.g. identity.Tenant: it isn't owned by some other, already-
// existing tenant -- it IS the tenant). The id is minted up front and used
// as the object's tenant_id too; the idempotency lookup, however, must use
// a namespace that's stable across repeated calls with the same name (a
// freshly-minted id is never stable), so it dedupes in a fixed "" namespace
// instead -- i.e. names are globally unique across all Tenants, not
// per-tenant-scoped the way VirtualMachine names are.
func (s *Store[T, PT]) CreateSelfReferential(ctx context.Context, name string, obj T) (T, error) {
	id := NewID(s.idPrefix)
	return s.create(id, id, "", name, obj)
}

func (s *Store[T, PT]) create(id, tenantID, dedupeNamespace, name string, obj T) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if name != "" {
		if existing, ok := s.byTenantName[dedupeNamespace+"/"+name]; ok {
			return s.byID[existing], nil
		}
	}

	p := PT(&obj)
	p.SetID(id)
	p.SetTenantID(tenantID)
	p.SetName(name)
	p.SetCreatedAt(time.Now())

	out := s.putLocked(obj, EventAdded)
	if name != "" {
		s.byTenantName[dedupeNamespace+"/"+name] = id
	}
	return out, nil
}

// Put is Create+Update combined, for resources whose id is a caller-chosen,
// stable identifier rather than something minted -- e.g. compute.Hypervisor,
// keyed by the hypervisor string operators/agents already use elsewhere
// (NATS subjects), not tenant-scoped, and re-registered (upserted) rather
// than idempotently returned unchanged. Unlike Update, it ignores
// resource_version entirely (there's no prior client-known value to
// optimistically check against a self-registering agent) and never
// conflicts: it creates on first call, overwrites on every later one,
// preserving CreatedAt across the overwrite. Emits ADDED the first time,
// MODIFIED after.
func (s *Store[T, PT]) Put(ctx context.Context, id, tenantID, name string, obj T) T {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := PT(&obj)
	p.SetID(id)
	p.SetTenantID(tenantID)
	p.SetName(name)

	eventType := EventAdded
	if current, ok := s.byID[id]; ok {
		p.SetCreatedAt(PT(&current).GetCreatedAt())
		eventType = EventModified
	} else {
		p.SetCreatedAt(time.Now())
	}

	out := s.putLocked(obj, eventType)
	if name != "" {
		s.byTenantName[tenantID+"/"+name] = id
	}
	return out
}

// LookupByName returns the object bound to (tenantID, name) by a prior
// Create, without minting anything -- the read-only half of Create's own
// idempotency check, exposed so a caller can run side effects (e.g. quota
// charging) only around a genuinely new Create, not an idempotent repeat.
func (s *Store[T, PT]) LookupByName(tenantID, name string) (T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var zero T
	if name == "" {
		return zero, false
	}
	id, ok := s.byTenantName[tenantID+"/"+name]
	if !ok {
		return zero, false
	}
	obj, ok := s.byID[id]
	return obj, ok
}

func (s *Store[T, PT]) Get(ctx context.Context, tenantID, id string) (T, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	obj, ok := s.byID[id]
	if !ok || PT(&obj).GetTenantID() != tenantID {
		var zero T
		return zero, s.errs.NotFound
	}
	return obj, nil
}

// List returns every object for tenantID, or every object across all
// tenants when tenantID is empty (internal use only; external callers must
// always pass their own tenant_id).
func (s *Store[T, PT]) List(ctx context.Context, tenantID string) ([]T, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []T
	for _, obj := range s.byID {
		if tenantID == "" || PT(&obj).GetTenantID() == tenantID {
			out = append(out, obj)
		}
	}
	return out, nil
}

// Update requires obj's resource_version to match the stored value
// (optimistic concurrency); mismatches return errs.Conflict.
//
// deleted_at is one-way (see Delete and docs/architecture.md "Finalizer"):
// once Delete has set it, this always keeps the stored value regardless of
// what obj carries -- a caller can shrink finalizers, but can never
// resurrect an object already marked for deletion. If that shrink empties
// finalizers, the object is actually removed here (emitting Deleted)
// instead of being stored (emitting Modified), the same way the
// finalizer-free path of Delete itself would have removed it immediately.
func (s *Store[T, PT]) Update(ctx context.Context, obj T) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := PT(&obj)
	current, ok := s.byID[p.GetID()]
	if !ok || PT(&current).GetTenantID() != p.GetTenantID() {
		var zero T
		return zero, s.errs.NotFound
	}
	if PT(&current).GetResourceVersion() != p.GetResourceVersion() {
		var zero T
		return zero, s.errs.Conflict
	}

	if deletedAt := PT(&current).GetDeletedAt(); deletedAt != nil {
		p.SetDeletedAt(deletedAt)
		if len(p.GetFinalizers()) == 0 {
			s.removeLocked(p)
			s.nextRV++
			p.SetResourceVersion(s.nextRV)
			s.emitLocked(Event[T]{Type: EventDeleted, Object: obj, ResourceVersion: s.nextRV})
			return obj, nil
		}
	}

	return s.putLocked(obj, EventModified), nil
}

// Delete removes the object immediately if it has no finalizers (the
// common case today -- unchanged from before finalizers existed). If it
// has finalizers, this instead marks it for deletion (sets deleted_at, if
// not already set) and emits Modified rather than Deleted: whoever holds a
// finalizer is expected to Watch for deleted_at appearing, do its cleanup,
// then call Update with its own entry removed from finalizers. Once the
// last one clears, Update performs the actual removal (see above). See
// docs/architecture.md "Finalizer".
func (s *Store[T, PT]) Delete(ctx context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	obj, ok := s.byID[id]
	p := PT(&obj)
	if !ok || p.GetTenantID() != tenantID {
		return s.errs.NotFound
	}

	if len(p.GetFinalizers()) == 0 {
		s.removeLocked(p)
		s.nextRV++
		p.SetResourceVersion(s.nextRV)
		s.emitLocked(Event[T]{Type: EventDeleted, Object: obj, ResourceVersion: s.nextRV})
		return nil
	}

	if p.GetDeletedAt() != nil {
		// Deletion is already in progress (a prior Delete call already set
		// this): a repeated call is a true no-op, not a second Modified
		// event over nothing actually new.
		return nil
	}
	now := time.Now()
	p.SetDeletedAt(&now)
	s.putLocked(obj, EventModified)
	return nil
}

// removeLocked deletes id from both indexes. Callers must hold s.mu and
// still need to bump nextRV/emit themselves (this alone doesn't produce an
// event, since Delete and Update need different ResourceVersion/Event
// values around it).
func (s *Store[T, PT]) removeLocked(p PT) {
	delete(s.byID, p.GetID())
	if p.GetName() != "" {
		delete(s.byTenantName, p.GetTenantID()+"/"+p.GetName())
	}
}

// Watch replays history newer than sinceRV (0 for "from the start") and
// then streams live events, both scoped to tenantID. An empty tenantID
// watches across all tenants, for internal use by a reconciler; external
// callers must always pass their own tenant_id. The returned channel is
// closed when ctx is done.
func (s *Store[T, PT]) Watch(ctx context.Context, tenantID string, sinceRV int64) (<-chan Event[T], error) {
	s.mu.Lock()
	if sinceRV > 0 && len(s.history) > 0 && sinceRV < s.history[0].ResourceVersion-1 {
		s.mu.Unlock()
		return nil, s.errs.HistoryPruned
	}

	var backlog []Event[T]
	for _, e := range s.history {
		obj := e.Object
		if e.ResourceVersion > sinceRV && (tenantID == "" || PT(&obj).GetTenantID() == tenantID) {
			backlog = append(backlog, e)
		}
	}

	ch := make(chan Event[T], 64)
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()

	out := make(chan Event[T], 64)
	go func() {
		defer close(out)
		defer s.removeWatcher(ch)

		for _, e := range backlog {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}

		ticker := time.NewTicker(s.bookmarkPeriod)
		defer ticker.Stop()
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					return
				}
				obj := e.Object
				if tenantID != "" && PT(&obj).GetTenantID() != tenantID {
					continue
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			case <-ticker.C:
				s.mu.RLock()
				rv := s.nextRV
				s.mu.RUnlock()
				select {
				case out <- Event[T]{Type: EventBookmark, ResourceVersion: rv}:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *Store[T, PT]) removeWatcher(ch chan Event[T]) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.watchers[ch]; ok {
		delete(s.watchers, ch)
		close(ch)
	}
}

// putLocked assigns the next resource_version, stores the object, and
// emits an event. Callers must hold s.mu.
func (s *Store[T, PT]) putLocked(obj T, eventType EventType) T {
	s.nextRV++
	p := PT(&obj)
	p.SetResourceVersion(s.nextRV)
	s.byID[p.GetID()] = obj
	s.emitLocked(Event[T]{Type: eventType, Object: obj, ResourceVersion: s.nextRV})
	return obj
}

func (s *Store[T, PT]) emitLocked(e Event[T]) {
	s.history = append(s.history, e)
	if len(s.history) > s.historyLimit {
		s.history = s.history[len(s.history)-s.historyLimit:]
	}
	for ch := range s.watchers {
		select {
		case ch <- e:
		default:
			// Slow watcher: drop it rather than blocking Create/Update/Delete.
			delete(s.watchers, ch)
			close(ch)
		}
	}
}
