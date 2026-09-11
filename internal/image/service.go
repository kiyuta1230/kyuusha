package image

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	clientv3 "go.etcd.io/etcd/client/v3"
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

func NewService(etcdClient *clientv3.Client) *Service {
	return &Service{
		store: resource.NewStore[Image, *Image](etcdClient, "image", resource.StoreErrors{
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
	if spec.Visibility == VisibilityUnspecified {
		spec.Visibility = VisibilityPrivate
	}
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
	switch spec.Visibility {
	case VisibilityPrivate, VisibilityPublic:
	default:
		return fmt.Errorf("%w: spec.visibility must be PRIVATE or PUBLIC", ErrValidation)
	}
	return nil
}

// visibleTo reports whether tenantID may Get/List/Watch/reference an Image
// it doesn't own: true for a VisibilityPublic Image (any tenant), or a
// VisibilityPrivate one that explicitly names tenantID in
// spec.shared_with_tenant_ids.
func visibleTo(spec Spec, tenantID string) bool {
	if spec.Visibility == VisibilityPublic {
		return true
	}
	for _, t := range spec.SharedWithTenantIDs {
		if t == tenantID {
			return true
		}
	}
	return false
}

// Get returns id if tenantID owns it, or -- since the generic
// resource.Store has no notion of cross-tenant visibility -- falls back to
// a scan for one it doesn't own but can still see (VisibilityPublic, or
// VisibilityPrivate shared with tenantID). The fallback never distinguishes
// "doesn't exist" from "exists but not visible to you": both return
// ErrNotFound, so a tenant can't probe for another tenant's private Image
// IDs.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Image, error) {
	if out, err := s.store.Get(ctx, tenantID, id); err == nil {
		return &out, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	all, err := s.store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, img := range all {
		if img.Meta.ID == id && visibleTo(img.Spec, tenantID) {
			return &img, nil
		}
	}
	return nil, ErrNotFound
}

// List returns every Image tenantID owns, plus every other tenant's Image
// it can see (see visibleTo) -- or every Image across all tenants,
// unfiltered, when tenantID is empty (internal use only; external callers
// must always pass their own tenant_id). Scans every Image regardless of
// tenantID rather than using resource.Store's own tenant-scoped List:
// fine at this system's target scale (a modest, mostly-shared image
// catalog, not VM-scale numbers -- see docs/architecture.md).
func (s *Service) List(ctx context.Context, tenantID string) ([]Image, error) {
	all, err := s.store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	if tenantID == "" {
		return all, nil
	}
	out := make([]Image, 0, len(all))
	for _, img := range all {
		if img.Meta.TenantID == tenantID || visibleTo(img.Spec, tenantID) {
			out = append(out, img)
		}
	}
	return out, nil
}

// SetVisibility replaces spec.visibility/shared_with_tenant_ids wholesale.
// Restricted to the owning tenantID regardless of the existing Visibility
// -- sharing only ever grants others read/reference access, never the
// ability to reshare or unshare it themselves. There's no separate Update
// RPC for Image (unlike VirtualMachine/Subnet): kernel/rootfs/disk URLs are
// meant to be immutable once Created (see docs/specs/image.md), so this is
// a narrow, purpose-built mutation rather than a general one, mirroring
// compute.Service.SetSchedulable.
func (s *Service) SetVisibility(ctx context.Context, tenantID, id string, visibility Visibility, sharedWithTenantIDs []string) (*Image, error) {
	if visibility == VisibilityUnspecified {
		visibility = VisibilityPrivate
	}
	switch visibility {
	case VisibilityPrivate, VisibilityPublic:
	default:
		return nil, fmt.Errorf("%w: visibility must be PRIVATE or PUBLIC", ErrValidation)
	}

	img, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	img.Spec.Visibility = visibility
	img.Spec.SharedWithTenantIDs = sharedWithTenantIDs
	out, err := s.store.Update(ctx, img)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.store.Delete(ctx, tenantID, id)
}

// Watch replays history newer than sinceRV (0 for "from the start") and then
// streams live events for tenantID's own Images plus every other tenant's
// Image it can see (see visibleTo), by watching every tenant's events (the
// generic resource.Store has no notion of cross-tenant visibility to watch
// selectively) and filtering here. An empty tenantID watches across all
// tenants unfiltered, for internal use by Run; external callers must
// always pass their own tenant_id. The returned channel is closed when ctx
// is done.
func (s *Service) Watch(ctx context.Context, tenantID string, sinceRV int64) (<-chan Event, error) {
	upstream, err := s.store.Watch(ctx, "", sinceRV, nil)
	if err != nil {
		return nil, err
	}
	if tenantID == "" {
		return upstream, nil
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		for e := range upstream {
			if e.Type != EventBookmark && e.Object.Meta.TenantID != tenantID && !visibleTo(e.Object.Spec, tenantID) {
				continue
			}
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Run blocks, watching for newly-Created (Pending) Images and checking
// their artifact URL(s) for reachability, until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	events, err := s.store.Watch(ctx, "", 0, nil) // all tenants: internal use only
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
