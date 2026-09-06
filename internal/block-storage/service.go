package blockstorage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
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
// There is no real StorageBackend yet (see docs/specs/volume.md) -- Volume
// Create goes straight to Ready, and VolumeAttachment Create goes straight
// to Attached once the exclusivity check passes; both skip the
// Pending/Attaching phases that a real backend would need time in.
//
// usageMu mirrors compute's identical field: it serializes the whole
// "look up idempotency, fetch quota, check, charge, create" sequence
// globally, across all tenants.
type Service struct {
	volumes     *resource.Store[Volume, *Volume]
	attachments *resource.Store[VolumeAttachment, *VolumeAttachment]

	identityClient identityv1.TenantServiceClient
	quota          *quotaChecker

	usageMu sync.Mutex
	usage   map[string]tenantUsage
}

func NewService(ctx context.Context, identityClient identityv1.TenantServiceClient) (*Service, error) {
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
		identityClient: identityClient,
		quota:          quota,
		usage:          make(map[string]tenantUsage),
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

	// No real StorageBackend yet (see docs/specs/volume.md): Create goes
	// straight to Ready, unlike NetworkInterface/Subnet's Pending-with-
	// pool-exhaustion path -- there's no pool here to exhaust yet, just a
	// Quota check already passed above.
	out, err := s.volumes.Create(ctx, tenantID, name, Volume{
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

// DeleteVolume releases the Volume's size_gb from tenant_usage in the same
// critical section, mirroring compute's identical Delete.
func (s *Service) DeleteVolume(ctx context.Context, tenantID, id string) error {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	vol, err := s.volumes.Get(ctx, tenantID, id)
	if err != nil {
		return err
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
	return s.volumes.Watch(ctx, tenantID, sinceRV)
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

	a.Status.Phase = VolumeAttachmentPhaseAttached
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
		if a.Status.Phase != VolumeAttachmentPhaseDeleting {
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

func (s *Service) DeleteVolumeAttachment(ctx context.Context, tenantID, id string) error {
	return s.attachments.Delete(ctx, tenantID, id)
}

func (s *Service) WatchVolumeAttachments(ctx context.Context, tenantID string, sinceRV int64) (<-chan VolumeAttachmentEvent, error) {
	return s.attachments.Watch(ctx, tenantID, sinceRV)
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
