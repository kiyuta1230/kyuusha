package blockstorage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	storageagentv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/storageagent/v1"
)

var (
	ErrVolumeNotFound      = errors.New("volume: not found")
	ErrVolumeConflict      = errors.New("volume: resource_version conflict")
	ErrVolumeHistoryPruned = errors.New("volume: watch resume point too old, relist required")

	ErrVolumeAttachmentNotFound      = errors.New("volume_attachment: not found")
	ErrVolumeAttachmentConflict      = errors.New("volume_attachment: resource_version conflict")
	ErrVolumeAttachmentHistoryPruned = errors.New("volume_attachment: watch resume point too old, relist required")

	ErrValidation    = errors.New("block-storage: validation failed")
	ErrQuotaExceeded = errors.New("block-storage: tenant quota exceeded")
)

type EventType = resource.EventType
type VolumeEvent = resource.Event[Volume]
type VolumeAttachmentEvent = resource.Event[VolumeAttachment]

const (
	EventAdded    = resource.EventAdded
	EventModified = resource.EventModified
	EventDeleted  = resource.EventDeleted
	EventBookmark = resource.EventBookmark
)

// pendingSweepInterval mirrors network's identical constant: a
// VolumeAttachment that couldn't attach at Create time (another active
// attachment already holds its volume_id -- see docs/architecture.md
// "具体的な排他制御") only gets retried when that old attachment clears,
// which doesn't itself touch the Pending one waiting for it. This periodic
// sweep is that retry.
const pendingSweepInterval = 10 * time.Second

// Service implements the VolumeService/VolumeAttachmentService CRUD+Watch
// surface against in-memory resource.Stores, plus Create-time Quota
// enforcement (max_volume_gb, see "Quota設計") and the exclusive-attach
// constraint from docs/architecture.md "未解決の危険: フェンシング問題": at
// most one non-Deleting VolumeAttachment may exist per volume_id at a time.
// The real StorageBackend (internal/storage-agent, one static node --
// see docs/specs/volume.md) is driven synchronously: Volume Create/Delete
// and VolumeAttachment attach/detach all call it directly rather than
// going through a Pending/Attaching phase with async retries, since a ZFS
// zvol/iSCSI export operation is fast. A storage-agent failure surfaces as
// an immediate error (Create) or an Error-phase Condition (attach), not a
// blocked-forever Pending -- that phase is reserved for the exclusive-
// attach wait, a genuinely different (and resolvable-by-waiting) situation.
//
// usageMu mirrors compute's identical field: it serializes the whole
// "look up idempotency, fetch quota, check, charge, create" sequence
// globally, across all tenants.
type Service struct {
	volumes     *resource.Store[Volume, *Volume]
	attachments *resource.Store[VolumeAttachment, *VolumeAttachment]

	identityClient     identityv1.TenantServiceClient
	storageAgentClient storageagentv1.StorageBackendServiceClient
	quota              *quotaChecker

	usageMu sync.Mutex
	usage   map[string]tenantUsage

	// attachMu serializes tryAttach's own check-then-act (hasActiveAttachment
	// followed by ExportVolume+Update to Attached): found live, two
	// VolumeAttachments created back-to-back for the same volume_id could
	// both call hasActiveAttachment before either had committed Attached,
	// both see "not blocked", and both attach -- exactly the exclusive-
	// attach constraint this is supposed to prevent. A global lock is fine
	// at this system's target scale (see hasActiveAttachment's own doc
	// comment) and keeps the fix in the one place both call sites
	// (CreateVolumeAttachment and retryPendingAttachments) already funnel
	// through.
	attachMu sync.Mutex
}

func NewService(ctx context.Context, identityClient identityv1.TenantServiceClient, storageAgentClient storageagentv1.StorageBackendServiceClient) (*Service, error) {
	quota, err := newQuotaChecker(ctx)
	if err != nil {
		return nil, err
	}
	return &Service{
		volumes: resource.NewStore[Volume, *Volume]("volume", resource.StoreErrors{
			NotFound:      ErrVolumeNotFound,
			Conflict:      ErrVolumeConflict,
			HistoryPruned: ErrVolumeHistoryPruned,
		}),
		attachments: resource.NewStore[VolumeAttachment, *VolumeAttachment]("volattach", resource.StoreErrors{
			NotFound:      ErrVolumeAttachmentNotFound,
			Conflict:      ErrVolumeAttachmentConflict,
			HistoryPruned: ErrVolumeAttachmentHistoryPruned,
		}),
		identityClient:     identityClient,
		storageAgentClient: storageAgentClient,
		quota:              quota,
		usage:              make(map[string]tenantUsage),
	}, nil
}

// Run retries Pending VolumeAttachments (blocked by another active
// attachment on the same volume_id at Create time) every
// pendingSweepInterval until ctx is done. Safe to call from only one
// goroutine; cmd/block-storage/main.go starts it once at startup.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(pendingSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.retryPendingAttachments(ctx)
		}
	}
}

func (s *Service) retryPendingAttachments(ctx context.Context) {
	attachments, err := s.attachments.List(ctx, "")
	if err != nil {
		return
	}
	for i := range attachments {
		if attachments[i].Status.Phase == VolumeAttachmentPhasePending {
			s.tryAttach(ctx, &attachments[i])
		}
	}
}

// CreateVolume is idempotent when name is set (same shape as compute's
// VirtualMachine Create): a second Create with the same (tenantID, name)
// returns the existing Volume rather than erroring or re-charging quota. A
// genuinely new Create synchronously checks tenantID's Quota (max_volume_gb)
// against block-storage's own local tenant_usage and rejects with
// ErrQuotaExceeded if it would be exceeded -- a doomed Volume is never
// created just to be marked Error afterwards.
func (s *Service) CreateVolume(ctx context.Context, tenantID, name string, spec VolumeSpec) (*Volume, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if spec.SizeGB <= 0 {
		return nil, fmt.Errorf("%w: spec.size_gb must be positive", ErrValidation)
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if existing, ok := s.volumes.LookupByName(tenantID, name); ok {
		return &existing, nil
	}

	limit, err := lookupQuota(ctx, s.identityClient, tenantID)
	if err != nil {
		return nil, err
	}
	usage := s.usage[tenantID]
	allowed, err := s.quota.allow(ctx, usage, spec.SizeGB, limit)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("%w: tenant %q", ErrQuotaExceeded, tenantID)
	}

	// Reserve the Volume's id before creating the real zvol: storage-agent
	// is keyed by volume_id (see internal/storage-agent), and
	// resource.Store assigns that id itself on Create, so a real backend
	// call has to happen either before (with a self-picked id) or after
	// (risking an orphaned zvol if the local Create then fails) the local
	// record exists. Picking the id here and passing it through avoids
	// both: a failed storage-agent call means nothing was ever created
	// locally either, and a Create that fails after (extremely unlikely,
	// see resource.Store) just orphans an empty zvol with no metadata
	// pointing at it, which DeleteVolume can never be told to clean up
	// anyway -- an acceptable v1 gap over the alternative.
	volumeID := resource.NewID("volume")
	if err := s.callStorageAgentCreateVolume(ctx, volumeID, spec.SizeGB); err != nil {
		return nil, fmt.Errorf("%w: storage backend: %v", ErrValidation, err)
	}

	out, err := s.volumes.CreateWithID(ctx, volumeID, tenantID, name, Volume{
		Spec:   spec,
		Status: VolumeStatus{Phase: VolumePhaseReady},
	})
	if err != nil {
		return nil, err
	}

	usage.VolumeGB += spec.SizeGB
	s.usage[tenantID] = usage

	return &out, nil
}

func (s *Service) callStorageAgentCreateVolume(ctx context.Context, volumeID string, sizeGB int64) error {
	_, err := s.storageAgentClient.CreateVolume(ctx, &storageagentv1.CreateVolumeRequest{VolumeId: volumeID, SizeGb: sizeGB})
	return err
}

func (s *Service) GetVolume(ctx context.Context, tenantID, id string) (*Volume, error) {
	out, err := s.volumes.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListVolumes(ctx context.Context, tenantID string) ([]Volume, error) {
	return s.volumes.List(ctx, tenantID)
}

// DeleteVolume rejects a Volume still referenced by a non-Deleting
// VolumeAttachment (deleting its zvol out from under a live iSCSI export
// would fail at the ZFS level anyway -- this just gives that failure a
// clear message instead of an opaque storage-agent error), calls
// storage-agent for real to destroy the zvol, and only then deletes the
// local record and releases the Volume's size_gb from tenant_usage in the
// same critical section, mirroring compute's identical Delete. Real
// backend first, metadata second: a failed storage-agent call leaves the
// Volume record intact for a retry, rather than deleting kyuusha's only
// record of a zvol that still exists.
func (s *Service) DeleteVolume(ctx context.Context, tenantID, id string) error {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	vol, err := s.volumes.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if blocked, err := s.hasActiveAttachment(ctx, id, ""); err != nil {
		return err
	} else if blocked {
		return fmt.Errorf("%w: volume %q still has an active VolumeAttachment", ErrValidation, id)
	}
	if _, err := s.storageAgentClient.DeleteVolume(ctx, &storageagentv1.DeleteVolumeRequest{VolumeId: id}); err != nil {
		return fmt.Errorf("%w: storage backend: %v", ErrValidation, err)
	}
	if err := s.volumes.Delete(ctx, tenantID, id); err != nil {
		return err
	}

	usage := s.usage[tenantID]
	usage.VolumeGB -= vol.Spec.SizeGB
	s.usage[tenantID] = usage

	return nil
}

func (s *Service) WatchVolumes(ctx context.Context, tenantID string, sinceRV int64) (<-chan VolumeEvent, error) {
	return s.volumes.Watch(ctx, tenantID, sinceRV, nil)
}

// CreateVolumeAttachment validates spec.volume_id against an existing,
// Ready Volume in the same tenant (same "never create a resource that
// references something that can't back it" rule as network's
// CreateNetworkInterface), then enforces the exclusive-attach constraint
// from docs/architecture.md "具体的な排他制御": if another non-Deleting
// VolumeAttachment already holds this volume_id, the new one is created
// Pending with a WaitingForOldAttachmentRelease Condition instead of being
// rejected -- the old one may well be in the middle of a normal Detach, not
// stuck -- and Run's periodic sweep retries it once that clears.
func (s *Service) CreateVolumeAttachment(ctx context.Context, tenantID, name string, spec VolumeAttachmentSpec) (*VolumeAttachment, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if spec.VMID == "" {
		return nil, fmt.Errorf("%w: spec.vm_id is required", ErrValidation)
	}
	if spec.VolumeID == "" {
		return nil, fmt.Errorf("%w: spec.volume_id is required", ErrValidation)
	}

	if existing, ok := s.attachments.LookupByName(tenantID, name); ok {
		return &existing, nil
	}

	vol, err := s.volumes.Get(ctx, tenantID, spec.VolumeID)
	if err != nil {
		if errors.Is(err, ErrVolumeNotFound) {
			return nil, fmt.Errorf("%w: volume_id %q does not exist", ErrValidation, spec.VolumeID)
		}
		return nil, err
	}
	if vol.Status.Phase != VolumePhaseReady {
		return nil, fmt.Errorf("%w: volume %q is not Ready (phase=%s)", ErrValidation, spec.VolumeID, vol.Status.Phase)
	}

	out, err := s.attachments.Create(ctx, tenantID, name, VolumeAttachment{
		Spec:   spec,
		Status: VolumeAttachmentStatus{Phase: VolumeAttachmentPhasePending},
	})
	if err != nil {
		return nil, err
	}
	s.tryAttach(ctx, &out)
	return &out, nil
}

// tryAttach mutates a in place: Attached on success (no other non-Deleting
// VolumeAttachment currently holds a.Spec.VolumeID), or still Pending with
// a WaitingForOldAttachmentRelease Condition otherwise (retried later by
// Run's sweep).
func (s *Service) tryAttach(ctx context.Context, a *VolumeAttachment) {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()

	blocked, err := s.hasActiveAttachment(ctx, a.Spec.VolumeID, a.Meta.ID)
	if err != nil {
		return
	}
	if blocked {
		a.Status.Conditions = upsertCondition(a.Status.Conditions, resource.Condition{
			Type: "WaitingForOldAttachmentRelease", Status: resource.ConditionTrue, LastTransitionAt: time.Now(),
		})
		if updated, err := s.attachments.Update(ctx, *a); err == nil {
			*a = updated
		}
		return
	}

	// Real StorageBackend call: makes a.Spec.VolumeID's zvol reachable over
	// iSCSI for whichever compute-agent ends up booting the VM (see
	// docs/specs/volume.md -- no per-hypervisor ACL yet, known gap). A
	// failure here is a different situation than "blocked": it's not
	// something waiting will resolve, so it goes to Error rather than
	// staying Pending for the sweep to retry forever.
	export, err := s.storageAgentClient.ExportVolume(ctx, &storageagentv1.ExportVolumeRequest{VolumeId: a.Spec.VolumeID})
	if err != nil {
		slog.Error("volume attachment: export volume failed", "attachment_id", a.Meta.ID, "volume_id", a.Spec.VolumeID, "err", err)
		a.Status.Phase = VolumeAttachmentPhaseError
		a.Status.Conditions = upsertCondition(a.Status.Conditions, resource.Condition{
			Type: "ExportFailed", Status: resource.ConditionTrue, Message: err.Error(), LastTransitionAt: time.Now(),
		})
		if updated, uerr := s.attachments.Update(ctx, *a); uerr == nil {
			*a = updated
		}
		return
	}

	a.Status.Phase = VolumeAttachmentPhaseAttached
	a.Status.TargetIQN = export.GetTargetIqn()
	a.Status.TargetPortal = export.GetPortal()
	a.Status.Conditions = upsertCondition(a.Status.Conditions, resource.Condition{
		Type: "WaitingForOldAttachmentRelease", Status: resource.ConditionFalse, LastTransitionAt: time.Now(),
	})
	if updated, err := s.attachments.Update(ctx, *a); err == nil {
		*a = updated
	}
}

// hasActiveAttachment reports whether some VolumeAttachment other than
// excludeID already holds volumeID in a non-Deleting phase -- the "at most
// one active attachment per volume_id" constraint. Scans every tenant's
// attachments (this Service, unlike network's per-Subnet IP pools, has no
// natural per-volume index to key off of): fine at this system's target
// scale (a modest number of live attachments, not VM-scale numbers).
func (s *Service) hasActiveAttachment(ctx context.Context, volumeID, excludeID string) (bool, error) {
	all, err := s.attachments.List(ctx, "")
	if err != nil {
		return false, err
	}
	for _, a := range all {
		if a.Meta.ID == excludeID || a.Spec.VolumeID != volumeID {
			continue
		}
		// Deleting: already releasing, doesn't hold the volume. Error: its
		// ExportVolume call never actually succeeded (see tryAttach), so it
		// never really held the volume either -- without this exclusion, an
		// attachment that failed once (e.g. a transient storage-agent
		// connectivity blip) would block every future attempt to attach
		// this volume forever, since nothing ever retries an Error-phase
		// attachment (only Pending ones, via Run's sweep).
		if a.Status.Phase != VolumeAttachmentPhaseDeleting && a.Status.Phase != VolumeAttachmentPhaseError {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) GetVolumeAttachment(ctx context.Context, tenantID, id string) (*VolumeAttachment, error) {
	out, err := s.attachments.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListVolumeAttachments(ctx context.Context, tenantID string) ([]VolumeAttachment, error) {
	return s.attachments.List(ctx, tenantID)
}

// DeleteVolumeAttachment unexports its Volume for real (best-effort: an
// attachment that never made it past Pending was never exported, and
// UnexportVolume is itself a no-op for a volume_id that isn't currently
// exported -- see internal/storage-agent) before removing the local
// record.
func (s *Service) DeleteVolumeAttachment(ctx context.Context, tenantID, id string) error {
	a, err := s.attachments.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if _, err := s.storageAgentClient.UnexportVolume(ctx, &storageagentv1.UnexportVolumeRequest{VolumeId: a.Spec.VolumeID}); err != nil {
		return fmt.Errorf("%w: storage backend: %v", ErrValidation, err)
	}
	return s.attachments.Delete(ctx, tenantID, id)
}

func (s *Service) WatchVolumeAttachments(ctx context.Context, tenantID string, sinceRV int64) (<-chan VolumeAttachmentEvent, error) {
	return s.attachments.Watch(ctx, tenantID, sinceRV, nil)
}

// upsertCondition mirrors network/reconciler.go's identical helper:
// Conditions accumulate history by type rather than growing unboundedly on
// every repeated retry.
func upsertCondition(conditions []resource.Condition, next resource.Condition) []resource.Condition {
	for i, c := range conditions {
		if c.Type == next.Type {
			conditions[i] = next
			return conditions
		}
	}
	return append(conditions, next)
}
