package compute

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

var (
	ErrNotFound      = errors.New("vm: not found")
	ErrConflict      = errors.New("vm: resource_version conflict")
	ErrValidation    = errors.New("vm: validation failed")
	ErrHistoryPruned = errors.New("vm: watch resume point too old, relist required")
)

type EventType string

const (
	EventAdded    EventType = "ADDED"
	EventModified EventType = "MODIFIED"
	EventDeleted  EventType = "DELETED"
	EventBookmark EventType = "BOOKMARK"
)

type Event struct {
	Type            EventType
	VM              VirtualMachine
	ResourceVersion int64
}

// Service implements the VirtualMachineService CRUD+Watch surface from
// docs/architecture.md against an in-memory store. This is the first,
// scheduler/NATS-free slice: it validates the shape of the declarative
// resource model (ObjectMeta, resource_version, Watch) end to end.
type Service struct {
	mu             sync.RWMutex
	byID           map[string]*VirtualMachine
	byTenantName   map[string]string // "<tenantID>/<name>" -> id, for idempotent Create
	history        []Event
	historyLimit   int
	nextRV         int64
	watchers       map[chan Event]struct{}
	bookmarkPeriod time.Duration
}

func NewService() *Service {
	return &Service{
		byID:           make(map[string]*VirtualMachine),
		byTenantName:   make(map[string]string),
		historyLimit:   1000,
		watchers:       make(map[chan Event]struct{}),
		bookmarkPeriod: 30 * time.Second,
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

	s.mu.Lock()
	defer s.mu.Unlock()

	if name != "" {
		key := tenantID + "/" + name
		if id, ok := s.byTenantName[key]; ok {
			existing := *s.byID[id]
			return &existing, nil
		}
	}

	now := time.Now()
	m := &VirtualMachine{
		Meta: resource.ObjectMeta{
			ID:        resource.NewID("vm"),
			Name:      name,
			TenantID:  tenantID,
			CreatedAt: now,
		},
		Spec: spec,
		Status: VirtualMachineStatus{
			Phase: PhasePending,
		},
	}
	s.putLocked(m, EventAdded)
	if name != "" {
		s.byTenantName[tenantID+"/"+name] = m.Meta.ID
	}

	out := *m
	return &out, nil
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (*VirtualMachine, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	m, ok := s.byID[id]
	if !ok || m.Meta.TenantID != tenantID {
		return nil, ErrNotFound
	}
	out := *m
	return &out, nil
}

func (s *Service) List(ctx context.Context, tenantID string) ([]VirtualMachine, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []VirtualMachine
	for _, m := range s.byID {
		if m.Meta.TenantID == tenantID {
			out = append(out, *m)
		}
	}
	return out, nil
}

// Update requires machine.Meta.ResourceVersion to match the stored value
// (optimistic concurrency); mismatches return ErrConflict.
func (s *Service) Update(ctx context.Context, machine *VirtualMachine) (*VirtualMachine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.byID[machine.Meta.ID]
	if !ok || current.Meta.TenantID != machine.Meta.TenantID {
		return nil, ErrNotFound
	}
	if current.Meta.ResourceVersion != machine.Meta.ResourceVersion {
		return nil, ErrConflict
	}

	updated := *machine
	s.putLocked(&updated, EventModified)

	out := updated
	return &out, nil
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.byID[id]
	if !ok || m.Meta.TenantID != tenantID {
		return ErrNotFound
	}
	delete(s.byID, id)
	if m.Meta.Name != "" {
		delete(s.byTenantName, tenantID+"/"+m.Meta.Name)
	}
	s.nextRV++
	m.Meta.ResourceVersion = s.nextRV
	s.emitLocked(Event{Type: EventDeleted, VM: *m, ResourceVersion: s.nextRV})
	return nil
}

// Watch replays history newer than sinceRV (0 for "from the start") and then
// streams live events, both scoped to tenantID. The returned channel is
// closed when ctx is done.
func (s *Service) Watch(ctx context.Context, tenantID string, sinceRV int64) (<-chan Event, error) {
	s.mu.Lock()
	if sinceRV > 0 && len(s.history) > 0 && sinceRV < s.history[0].ResourceVersion-1 {
		s.mu.Unlock()
		return nil, ErrHistoryPruned
	}

	var backlog []Event
	for _, e := range s.history {
		if e.ResourceVersion > sinceRV && e.VM.Meta.TenantID == tenantID {
			backlog = append(backlog, e)
		}
	}

	ch := make(chan Event, 64)
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()

	out := make(chan Event, 64)
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
				if e.VM.Meta.TenantID != tenantID {
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
				case out <- Event{Type: EventBookmark, ResourceVersion: rv}:
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

func (s *Service) removeWatcher(ch chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.watchers[ch]; ok {
		delete(s.watchers, ch)
		close(ch)
	}
}

// putLocked assigns the next resource_version, stores the object, and emits
// an event. Callers must hold s.mu.
func (s *Service) putLocked(m *VirtualMachine, eventType EventType) {
	s.nextRV++
	m.Meta.ResourceVersion = s.nextRV
	stored := *m
	s.byID[m.Meta.ID] = &stored
	s.emitLocked(Event{Type: eventType, VM: *m, ResourceVersion: s.nextRV})
}

func (s *Service) emitLocked(e Event) {
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
