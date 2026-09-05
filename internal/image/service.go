package image

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

var (
	ErrNotFound      = errors.New("image: not found")
	ErrConflict      = errors.New("image: resource_version conflict")
	ErrValidation    = errors.New("image: validation failed")
	ErrHistoryPruned = errors.New("image: watch resume point too old, relist required")
)

type EventType = resource.EventType
type Event = resource.Event[Image]

const (
	EventAdded    = resource.EventAdded
	EventModified = resource.EventModified
	EventDeleted  = resource.EventDeleted
	EventBookmark = resource.EventBookmark
)

// reachabilityTimeout bounds each artifact URL's HTTP HEAD check.
const reachabilityTimeout = 5 * time.Second

// Service implements the ImageService CRUD+Watch surface. Unlike
// VirtualMachine there's no scheduler/NATS side; the only asynchronous work
// is the Pending->Ready reachability check, run by Run below.
type Service struct {
	store      *resource.Store[Image, *Image]
	httpClient *http.Client
}

func NewService() *Service {
	return &Service{
		store: resource.NewStore[Image, *Image]("image", resource.StoreErrors{
			NotFound:      ErrNotFound,
			Conflict:      ErrConflict,
			HistoryPruned: ErrHistoryPruned,
		}),
		httpClient: &http.Client{Timeout: reachabilityTimeout},
	}
}

// Create validates spec.format matches the artifacts actually provided
// (docs/architecture.md's Create-time validation) and starts the Image in
// Pending; Run's background check flips it to Ready or Error once the
// artifact URL(s) are confirmed reachable (or not).
func (s *Service) Create(ctx context.Context, tenantID, name string, spec Spec) (*Image, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}

	out, err := s.store.Create(ctx, tenantID, name, Image{
		Spec:   spec,
		Status: Status{Phase: PhasePending},
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func validateSpec(spec Spec) error {
	switch spec.Format {
	case FormatKernelRootfs:
		if spec.Kernel.URL == "" || spec.Rootfs.URL == "" {
			return fmt.Errorf("%w: KERNEL_ROOTFS requires both kernel.url and rootfs.url", ErrValidation)
		}
	case FormatQCOW2:
		if spec.Disk.URL == "" {
			return fmt.Errorf("%w: QCOW2 requires disk.url", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: spec.format must be set", ErrValidation)
	}
	return nil
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (*Image, error) {
	out, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every Image for tenantID, or every Image across all tenants
// when tenantID is empty (internal use only; external callers must always
// pass their own tenant_id).
func (s *Service) List(ctx context.Context, tenantID string) ([]Image, error) {
	return s.store.List(ctx, tenantID)
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.store.Delete(ctx, tenantID, id)
}

// Watch replays history newer than sinceRV (0 for "from the start") and then
// streams live events, both scoped to tenantID. An empty tenantID watches
// across all tenants, for internal use by Run; external callers must
// always pass their own tenant_id. The returned channel is closed when ctx
// is done.
func (s *Service) Watch(ctx context.Context, tenantID string, sinceRV int64) (<-chan Event, error) {
	return s.store.Watch(ctx, tenantID, sinceRV)
}

// Run blocks, watching for newly-Created (Pending) Images and checking
// their artifact URL(s) for reachability, until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	events, err := s.store.Watch(ctx, "", 0) // all tenants: internal use only
	if err != nil {
		return fmt.Errorf("watch images: %w", err)
	}
	for e := range events {
		if e.Type != EventAdded {
			continue
		}
		if e.Object.Status.Phase == PhasePending {
			go s.checkReachability(ctx, e.Object)
		}
	}
	return nil
}

func (s *Service) checkReachability(ctx context.Context, img Image) {
	urls := artifactURLs(img.Spec)
	var unreachable string
	for _, u := range urls {
		if err := s.headCheck(ctx, u); err != nil {
			unreachable = fmt.Sprintf("%s: %v", u, err)
			break
		}
	}

	updated := img
	now := time.Now()
	if unreachable != "" {
		updated.Status.Phase = PhaseError
		updated.Status.Conditions = append(updated.Status.Conditions, resource.Condition{
			Type: "URLUnreachable", Status: resource.ConditionTrue,
			Message: unreachable, LastTransitionAt: now,
		})
	} else {
		updated.Status.Phase = PhaseReady
	}

	if _, err := s.store.Update(ctx, updated); err != nil {
		slog.Error("image: reachability check: update failed", "image_id", img.Meta.ID, "err", err)
	}
}

func (s *Service) headCheck(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, reachabilityTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HEAD returned %d", resp.StatusCode)
	}
	return nil
}

func artifactURLs(spec Spec) []string {
	switch spec.Format {
	case FormatKernelRootfs:
		return []string{spec.Kernel.URL, spec.Rootfs.URL}
	case FormatQCOW2:
		return []string{spec.Disk.URL}
	default:
		return nil
	}
}
