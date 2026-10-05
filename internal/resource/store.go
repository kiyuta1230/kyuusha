package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
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
// every kyuusha resource type, backed by etcd (see docs/architecture.md
// 「採用: バッキングストアにetcdを採用する」, 2026-09-11 -- an earlier version
// of this file was purely in-memory, losing all state on every restart;
// that was a genuine bug, not a deliberate scope decision).
//
// ObjectMeta.ResourceVersion is etcd's own per-key mod_revision, not a
// separately-tracked counter: it's never written into the serialized JSON
// value, always overlaid from etcd's response metadata on every read
// (Get/List/Watch) -- this is what makes Update's optimistic-concurrency
// check a direct etcd Txn compare (see Update) rather than something this
// package has to arbitrate itself.
//
// Key layout: object bodies live under /kyuusha/obj/<idPrefix>/<tenant or
// "_">/<id>; the idempotent-by-name index lives separately under
// /kyuusha/name/<idPrefix>/<dedupeNamespace or "_">/<name> -> id, written
// atomically alongside the object in the same etcd Txn. T is the resource
// value type (e.g. compute.VirtualMachine); PT is its pointer type, which
// must satisfy Meta -- see that type's doc comment for the embedding
// pattern.
type Store[T any, PT interface {
	*T
	Meta
}] struct {
	client         *clientv3.Client
	idPrefix       string
	errs           StoreErrors
	bookmarkPeriod time.Duration
}

func NewStore[T any, PT interface {
	*T
	Meta
}](client *clientv3.Client, idPrefix string, errs StoreErrors) *Store[T, PT] {
	return &Store[T, PT]{
		client:         client,
		idPrefix:       idPrefix,
		errs:           errs,
		bookmarkPeriod: 30 * time.Second,
	}
}

// tenantSegment maps "" (a non-tenant-scoped resource, e.g. Hypervisor) to
// a fixed placeholder -- real tenant IDs are minted ("tenant-xxxxxxxx"),
// never user-chosen, so collision with the literal placeholder isn't a
// practical concern.
func tenantSegment(tenantID string) string {
	if tenantID == "" {
		return "_"
	}
	return tenantID
}

func (s *Store[T, PT]) objectKey(tenantID, id string) string {
	return fmt.Sprintf("/kyuusha/obj/%s/%s/%s", s.idPrefix, tenantSegment(tenantID), id)
}

func (s *Store[T, PT]) objectPrefix() string {
	return fmt.Sprintf("/kyuusha/obj/%s/", s.idPrefix)
}

func (s *Store[T, PT]) tenantPrefix(tenantID string) string {
	return fmt.Sprintf("/kyuusha/obj/%s/%s/", s.idPrefix, tenantSegment(tenantID))
}

func (s *Store[T, PT]) nameKey(dedupeNamespace, name string) string {
	return fmt.Sprintf("/kyuusha/name/%s/%s/%s", s.idPrefix, tenantSegment(dedupeNamespace), name)
}

// decode unmarshals an etcd value into T and overlays its real
// resource_version from modRevision (see the package doc comment -- never
// trust whatever ResourceVersion happens to be embedded in the stored
// JSON).
func (s *Store[T, PT]) decode(data []byte, modRevision int64) (T, error) {
	var obj T
	if err := json.Unmarshal(data, &obj); err != nil {
		return obj, err
	}
	PT(&obj).SetResourceVersion(modRevision)
	return obj, nil
}

// Create is idempotent when name is set: a second Create for the same
// (tenantID, name) returns the existing object rather than minting a new
// one. obj should already carry its domain Spec/Status; Create fills in
// Meta (ID, TenantID, Name, CreatedAt).
func (s *Store[T, PT]) Create(ctx context.Context, tenantID, name string, obj T) (T, error) {
	return s.create(ctx, NewID(s.idPrefix), tenantID, tenantID, name, obj)
}

// CreateSelfReferential is Create for resources whose own tenant_id equals
// their id (e.g. identity.Tenant: it isn't owned by some other, already-
// existing tenant -- it IS the tenant). The id is minted up front and used
// as the object's tenant_id too; the idempotency lookup, however, must use
// a namespace that's stable across repeated calls with the same name (a
// freshly-minted id is never stable), so it dedupes in a fixed ""
// (global) namespace instead -- i.e. names are globally unique across all
// Tenants, not per-tenant-scoped the way VirtualMachine names are.
func (s *Store[T, PT]) CreateSelfReferential(ctx context.Context, name string, obj T) (T, error) {
	id := NewID(s.idPrefix)
	return s.create(ctx, id, id, "", name, obj)
}

// create is a bounded retry loop (mirrors compute.Service's own
// updateHypervisor pattern) rather than a mutex: the in-memory
// implementation this replaced serialized the whole idempotency-check-then-
// write sequence under one lock, which etcd has no equivalent of. A
// concurrent Create racing for the same (dedupeNamespace, name) is instead
// caught by the Txn's compare failing, at which point the name index now
// really does exist and the retry's lookup finds (and returns) the winner.
func (s *Store[T, PT]) create(ctx context.Context, id, tenantID, dedupeNamespace, name string, obj T) (T, error) {
	var zero T
	for attempt := 0; attempt < 5; attempt++ {
		if name != "" {
			if existing, ok, err := s.lookupByName(ctx, dedupeNamespace, name); err != nil {
				return zero, err
			} else if ok {
				return existing, nil
			}
		}

		p := PT(&obj)
		p.SetID(id)
		p.SetTenantID(tenantID)
		p.SetName(name)
		p.SetCreatedAt(time.Now())

		data, err := json.Marshal(obj)
		if err != nil {
			return zero, err
		}

		objKey := s.objectKey(tenantID, id)
		cmps := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(objKey), "=", 0)}
		ops := []clientv3.Op{clientv3.OpPut(objKey, string(data))}
		if name != "" {
			nameKey := s.nameKey(dedupeNamespace, name)
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(nameKey), "=", 0))
			ops = append(ops, clientv3.OpPut(nameKey, encodeNameIndexValue(tenantID, id)))
		}

		resp, err := s.client.Txn(ctx).If(cmps...).Then(ops...).Commit()
		if err != nil {
			return zero, err
		}
		if resp.Succeeded {
			return s.Get(ctx, tenantID, id)
		}
		// Lost the race (name index now exists, written by a concurrent
		// Create) -- loop back and the lookupByName above will find it.
	}
	return zero, fmt.Errorf("resource: create %q: too many concurrent retries", s.idPrefix)
}

// Put is Create+Update combined, for resources whose id is a caller-chosen,
// stable identifier rather than something minted -- e.g. compute.Hypervisor,
// keyed by the hypervisor string operators/agents already use elsewhere
// (NATS subjects), not tenant-scoped, and re-registered (upserted) rather
// than idempotently returned unchanged. Unlike Update, it ignores
// resource_version entirely (there's no prior client-known value to
// optimistically check against a self-registering agent) and never
// conflicts: it creates on first call, overwrites on every later one,
// preserving CreatedAt across the overwrite.
func (s *Store[T, PT]) Put(ctx context.Context, id, tenantID, name string, obj T) (T, error) {
	var zero T
	current, err := s.Get(ctx, tenantID, id)
	exists := err == nil
	if err != nil && s.errs.NotFound != nil && err != s.errs.NotFound {
		return zero, err
	}

	p := PT(&obj)
	p.SetID(id)
	p.SetTenantID(tenantID)
	p.SetName(name)
	if exists {
		p.SetCreatedAt(PT(&current).GetCreatedAt())
	} else {
		p.SetCreatedAt(time.Now())
	}

	data, err := json.Marshal(obj)
	if err != nil {
		return zero, err
	}
	ops := []clientv3.Op{clientv3.OpPut(s.objectKey(tenantID, id), string(data))}
	if name != "" {
		ops = append(ops, clientv3.OpPut(s.nameKey(tenantID, name), encodeNameIndexValue(tenantID, id)))
	}
	if _, err := s.client.Txn(ctx).Then(ops...).Commit(); err != nil {
		return zero, err
	}
	return s.Get(ctx, tenantID, id)
}

// encodeNameIndexValue/decodeNameIndexValue pack (tenantID, id) into a
// name-index value. Both are needed to Get the object back: for a regular
// Create, tenantID always equals dedupeNamespace so a caller could in
// principle re-derive it -- but for CreateSelfReferential, dedupeNamespace
// is always "" while the object's real tenantID is its own freshly-minted
// id, different on every call, so it isn't recoverable from dedupeNamespace
// or from the calling context and must be stored alongside id directly.
func encodeNameIndexValue(tenantID, id string) string {
	return tenantID + "\x00" + id
}

func decodeNameIndexValue(value string) (tenantID, id string) {
	tenantID, id, _ = strings.Cut(value, "\x00")
	return tenantID, id
}

// lookupByName is LookupByName's implementation, parameterized on the
// dedupe namespace directly (LookupByName itself only ever exposes the
// per-tenant-namespace case to callers outside this package -- see its own
// doc comment).
func (s *Store[T, PT]) lookupByName(ctx context.Context, dedupeNamespace, name string) (T, bool, error) {
	var zero T
	if name == "" {
		return zero, false, nil
	}
	resp, err := s.client.Get(ctx, s.nameKey(dedupeNamespace, name))
	if err != nil {
		return zero, false, err
	}
	if len(resp.Kvs) == 0 {
		return zero, false, nil
	}
	tenantID, id := decodeNameIndexValue(string(resp.Kvs[0].Value))
	obj, err := s.Get(ctx, tenantID, id)
	if err != nil {
		if err == s.errs.NotFound {
			return zero, false, nil
		}
		return zero, false, err
	}
	return obj, true, nil
}

// LookupByName returns the object bound to (tenantID, name) by a prior
// Create, without minting anything -- the read-only half of Create's own
// idempotency check, exposed so a caller can run side effects (e.g. quota
// charging) only around a genuinely new Create, not an idempotent repeat.
// Only ever the per-tenant dedupe namespace (matching Create, not
// CreateSelfReferential -- see the package's own create()).
func (s *Store[T, PT]) LookupByName(ctx context.Context, tenantID, name string) (T, bool) {
	obj, ok, err := s.lookupByName(ctx, tenantID, name)
	if err != nil {
		var zero T
		return zero, false
	}
	return obj, ok
}

func (s *Store[T, PT]) Get(ctx context.Context, tenantID, id string) (T, error) {
	var zero T
	resp, err := s.client.Get(ctx, s.objectKey(tenantID, id))
	if err != nil {
		return zero, err
	}
	if len(resp.Kvs) == 0 {
		return zero, s.errs.NotFound
	}
	return s.decode(resp.Kvs[0].Value, resp.Kvs[0].ModRevision)
}

// List returns every object for tenantID, or every object across all
// tenants when tenantID is empty. An empty tenant_id from an external caller
// reaches here only after authz has allowed an unscoped request (cross-
// tenant roles only -- see docs/specs/authn-authz.md); that cross-tenant
// List/Watch is a supported contract, not an internal-only shortcut.
func (s *Store[T, PT]) List(ctx context.Context, tenantID string) ([]T, error) {
	out, _, err := s.ListWithRevision(ctx, tenantID)
	return out, err
}

// ListWithRevision is List plus the etcd revision the result is a
// consistent snapshot of: every change at or before it is reflected, none
// after it.
func (s *Store[T, PT]) ListWithRevision(ctx context.Context, tenantID string) ([]T, int64, error) {
	prefix := s.objectPrefix()
	if tenantID != "" {
		prefix = s.tenantPrefix(tenantID)
	}
	resp, err := s.client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, 0, err
	}
	out := make([]T, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		obj, err := s.decode(kv.Value, kv.ModRevision)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, obj)
	}
	return out, resp.Header.Revision, nil
}

// Update requires obj's resource_version to match the stored value
// (optimistic concurrency, enforced by an etcd Txn compare on the key's
// mod_revision -- not a separate check-then-act race window); mismatches
// return errs.Conflict.
//
// deleted_at is one-way (see Delete and docs/architecture.md "Finalizer"):
// once Delete has set it, this always keeps the stored value regardless of
// what obj carries -- a caller can shrink finalizers, but can never
// resurrect an object already marked for deletion. If that shrink empties
// finalizers, the object is actually removed here (emitting Deleted via
// Watch) instead of being stored (emitting Modified), the same way the
// finalizer-free path of Delete itself would have removed it immediately.
func (s *Store[T, PT]) Update(ctx context.Context, obj T) (T, error) {
	var zero T
	p := PT(&obj)
	tenantID := p.GetTenantID()
	id := p.GetID()
	objKey := s.objectKey(tenantID, id)

	current, err := s.Get(ctx, tenantID, id)
	if err != nil {
		return zero, err
	}

	finalRemoval := false
	if deletedAt := PT(&current).GetDeletedAt(); deletedAt != nil {
		p.SetDeletedAt(deletedAt)
		finalRemoval = len(p.GetFinalizers()) == 0
	}

	var ops []clientv3.Op
	if finalRemoval {
		ops = []clientv3.Op{clientv3.OpDelete(objKey)}
		if name := PT(&current).GetName(); name != "" {
			ops = append(ops, clientv3.OpDelete(s.nameKey(tenantID, name)))
		}
	} else {
		data, err := json.Marshal(obj)
		if err != nil {
			return zero, err
		}
		ops = []clientv3.Op{clientv3.OpPut(objKey, string(data))}
	}

	resp, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(objKey), "=", p.GetResourceVersion())).
		Then(ops...).
		Commit()
	if err != nil {
		return zero, err
	}
	if !resp.Succeeded {
		return zero, s.errs.Conflict
	}
	if finalRemoval {
		return obj, nil
	}
	return s.Get(ctx, tenantID, id)
}

// Delete removes the object immediately if it has no finalizers (the
// common case today -- unchanged from before finalizers existed). If it
// has finalizers, this instead marks it for deletion (sets deleted_at, if
// not already set) and emits Modified (via Watch) rather than Deleted:
// whoever holds a finalizer is expected to Watch for deleted_at appearing,
// do its cleanup, then call Update with its own entry removed from
// finalizers. Once the last one clears, Update performs the actual removal
// (see above). See docs/architecture.md "Finalizer".
//
// Internally retries a bounded number of times on a concurrent-
// modification race (same shape as compute.Service's updateHypervisor) --
// Delete's own contract has no Conflict to surface, so this absorbs it
// rather than exposing a new error case to every caller.
func (s *Store[T, PT]) Delete(ctx context.Context, tenantID, id string) error {
	objKey := s.objectKey(tenantID, id)
	for attempt := 0; attempt < 20; attempt++ {
		obj, err := s.Get(ctx, tenantID, id)
		if err != nil {
			return err
		}
		p := PT(&obj)

		if len(p.GetFinalizers()) == 0 {
			ops := []clientv3.Op{clientv3.OpDelete(objKey)}
			if p.GetName() != "" {
				ops = append(ops, clientv3.OpDelete(s.nameKey(tenantID, p.GetName())))
			}
			resp, err := s.client.Txn(ctx).
				If(clientv3.Compare(clientv3.ModRevision(objKey), "=", p.GetResourceVersion())).
				Then(ops...).
				Commit()
			if err != nil {
				return err
			}
			if !resp.Succeeded {
				continue // raced with a concurrent modification -- retry against fresh state
			}
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
		data, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		resp, err := s.client.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(objKey), "=", p.GetResourceVersion())).
			Then(clientv3.OpPut(objKey, string(data))).
			Commit()
		if err != nil {
			return err
		}
		if !resp.Succeeded {
			continue
		}
		return nil
	}
	return fmt.Errorf("resource: delete %q %q: too many concurrent retries", s.idPrefix, id)
}

// Watch replays every object matching tenantID/matches as of "now" (when
// sinceRV is 0) or every change since sinceRV (when resuming), then streams
// live events, both scoped the same way. An empty tenantID watches across
// all tenants -- used by reconcilers, and by external callers holding a
// cross-tenant role (same contract as List). The returned channel is closed when ctx
// is done, the underlying etcd watch ends, or sinceRV has already been
// compacted away (see errs.HistoryPruned, returned synchronously here the
// same way the old in-memory implementation did, not as a mid-stream
// surprise).
func (s *Store[T, PT]) Watch(ctx context.Context, tenantID string, sinceRV int64, matches func(T) bool) (<-chan Event[T], error) {
	prefix := s.objectPrefix()
	if tenantID != "" {
		prefix = s.tenantPrefix(tenantID)
	}

	var backlog []Event[T]
	startRev := sinceRV + 1
	if sinceRV == 0 {
		resp, err := s.client.Get(ctx, prefix, clientv3.WithPrefix())
		if err != nil {
			return nil, err
		}
		for _, kv := range resp.Kvs {
			obj, err := s.decode(kv.Value, kv.ModRevision)
			if err != nil {
				return nil, err
			}
			if matches == nil || matches(obj) {
				backlog = append(backlog, Event[T]{Type: EventAdded, Object: obj, ResourceVersion: kv.ModRevision})
			}
		}
		startRev = resp.Header.Revision + 1
	}

	// WithCreatedNotify guarantees the *first* response arrives immediately
	// (before any real mutation happens), whether that's a plain "watch
	// established" notice or -- if startRev has already been compacted away
	// -- a Canceled response. Without it, a quiet keyspace would leave the
	// synchronous probe below blocking indefinitely instead of confirming
	// either outcome right away.
	watchCh := s.client.Watch(ctx, prefix, clientv3.WithPrefix(), clientv3.WithRev(startRev), clientv3.WithPrevKV(), clientv3.WithCreatedNotify())

	var first clientv3.WatchResponse
	select {
	case wresp, ok := <-watchCh:
		if !ok {
			return nil, fmt.Errorf("resource: watch %q: channel closed immediately", s.idPrefix)
		}
		if wresp.Canceled {
			return nil, s.errs.HistoryPruned
		}
		first = wresp
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	out := make(chan Event[T], 64)
	go func() {
		defer close(out)

		for _, e := range backlog {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}

		ticker := time.NewTicker(s.bookmarkPeriod)
		defer ticker.Stop()
		lastRV := startRev - 1

		process := func(wresp clientv3.WatchResponse) bool {
			lastRV = wresp.Header.Revision
			for _, ev := range wresp.Events {
				var obj T
				var err error
				var eventType EventType
				switch {
				case ev.Type == mvccpb.DELETE:
					eventType = EventDeleted
					if ev.PrevKv == nil {
						continue
					}
					obj, err = s.decode(ev.PrevKv.Value, ev.Kv.ModRevision)
				case ev.IsCreate():
					eventType = EventAdded
					obj, err = s.decode(ev.Kv.Value, ev.Kv.ModRevision)
				default:
					eventType = EventModified
					obj, err = s.decode(ev.Kv.Value, ev.Kv.ModRevision)
				}
				if err != nil {
					continue
				}
				if matches != nil && !matches(obj) {
					continue
				}
				select {
				case out <- Event[T]{Type: eventType, Object: obj, ResourceVersion: ev.Kv.ModRevision}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		if !process(first) {
			return
		}

		for {
			select {
			case wresp, ok := <-watchCh:
				if !ok {
					return
				}
				if wresp.Canceled {
					return
				}
				if !process(wresp) {
					return
				}
			case <-ticker.C:
				select {
				case out <- Event[T]{Type: EventBookmark, ResourceVersion: lastRV}:
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
