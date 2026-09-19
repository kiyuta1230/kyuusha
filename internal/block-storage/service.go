package blockstorage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiyuta1230/kyuusha/internal/resource"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

var (
	ErrVolumeNotFound      = errors.New("volume: not found")
	ErrVolumeConflict      = errors.New("volume: resource_version conflict")
	ErrVolumeHistoryPruned = errors.New("volume: watch resume point too old, relist required")

	ErrVolumeAttachmentNotFound      = errors.New("volume_attachment: not found")
	ErrVolumeAttachmentConflict      = errors.New("volume_attachment: resource_version conflict")
	ErrVolumeAttachmentHistoryPruned = errors.New("volume_attachment: watch resume point too old, relist required")

	ErrStorageConnectionNotFound      = errors.New("storage_connection: not found")
	ErrStorageConnectionConflict      = errors.New("storage_connection: resource_version conflict")
	ErrStorageConnectionHistoryPruned = errors.New("storage_connection: watch resume point too old, relist required")

	ErrValidation    = errors.New("block-storage: validation failed")
	ErrQuotaExceeded = errors.New("block-storage: tenant quota exceeded")
)

type EventType = resource.EventType
type VolumeEvent = resource.Event[Volume]
type VolumeAttachmentEvent = resource.Event[VolumeAttachment]
type StorageConnectionEvent = resource.Event[StorageConnection]

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

// orphanSweepInterval implements docs/architecture.md's "孤児リソースGC"
// design decision (10-minute periodic sweep; a child checks its parent's
// existence via Get, deletes itself if NotFound) for VolumeAttachment --
// see docs/specs/volume.md's "VolumeAttachmentのオーファンGC". VM Delete
// (compute.Reconciler.releaseIfReserved) does actively delete every
// VolumeAttachment listed in VirtualMachineStatus.VolumeAttachmentRefs, so
// this is mostly a backstop for that fire-and-forget call failing -- except
// for a VolumeAttachment created directly against this service *after* its
// VM already booted (VolumeAttachmentRefs only ever holds the ones
// attach-before-boot resolved): that one was never tracked by
// releaseIfReserved at all, and would otherwise never be cleaned up by
// anything.
const orphanSweepInterval = 10 * time.Minute

// Service implements the VolumeService/VolumeAttachmentService CRUD+Watch
// surface against in-memory resource.Stores, plus Create-time Quota
// enforcement (max_volume_gb, see "Quota設計") and the exclusive-attach
// constraint from docs/architecture.md "未解決の危険: フェンシング問題": at
// most one non-Deleting/non-Error VolumeAttachment may exist per volume_id
// at a time.
//
// kyuusha never provisions or exports storage itself (see
// docs/architecture.md「訂正: 責務の境界を...」) -- Volume Create/Delete are
// pure metadata operations (no external backend call), and
// VolumeAttachment attach/detach is just the exclusive-attach bookkeeping.
// The actual block device/file a Volume references, and whichever
// Hypervisors can reach it, are entirely the operator's concern
// (docs/specs/volume.md).
//
// usageMu mirrors compute's identical field: it serializes the whole
// "look up idempotency, fetch quota, check, charge, create" sequence
// globally, across all tenants.
type Service struct {
	volumes            *resource.Store[Volume, *Volume]
	attachments        *resource.Store[VolumeAttachment, *VolumeAttachment]
	storageConnections *resource.Store[StorageConnection, *StorageConnection]

	identityClient identityv1.TenantServiceClient
	quota          *quotaChecker
	// computeClient is used only by sweepOrphanedVolumeAttachments, to ask
	// "does this VolumeAttachment's vm_id still exist" -- the one place
	// this service needs to know anything about a VM at all.
	computeClient computev1.VirtualMachineServiceClient

	usageMu sync.Mutex
	usage   map[string]tenantUsage

	// attachMu serializes tryAttach's own check-then-act (hasActiveAttachment
	// followed by Update to Attached): found live, two VolumeAttachments
	// created back-to-back for the same volume_id could both call
	// hasActiveAttachment before either had committed Attached, both see
	// "not blocked", and both attach -- exactly the exclusive-attach
	// constraint this is supposed to prevent. A global lock is fine at this
	// system's target scale (see hasActiveAttachment's own doc comment) and
	// keeps the fix in the one place both call sites (CreateVolumeAttachment
	// and retryPendingAttachments) already funnel through.
	attachMu sync.Mutex

	// nc/js are nil until Run starts (see verification.go) -- CreateVolume
	// and the sweeps all tolerate that (a test that never calls Run just
	// never gets a Volume past Pending via verification, which is fine:
	// tests that need a Ready Volume force it directly, see testing.go).
	nc *nats.Conn
	js jetstream.JetStream

	// hcMu guards hypervisorConnections, this Service's in-memory view of
	// "which Hypervisor, in which zone, self-reports which storage
	// connection names" -- rebuilt entirely from NATS events (see
	// verification.go's recordHypervisorConnections), never persisted.
	hcMu                  sync.Mutex
	hypervisorConnections map[string]hypervisorConnInfo

	// verifyBackoffMu guards verifyBackoff, this Service's in-memory
	// per-Volume exponential-backoff state for sweepPendingVolumes (see
	// verification.go) -- never persisted, rebuilt implicitly (starts back
	// at the initial interval) on restart, same tolerance rebuildUsage's
	// own doc comment accepts elsewhere for purely in-process state.
	verifyBackoffMu sync.Mutex
	verifyBackoff   map[string]verifyRetryState
}

// NewService constructs a Service and synchronously rebuilds its tenant
// quota usage (see rebuildUsage) from etcd before returning -- callers
// must not start serving Create requests until this returns, for the same
// "don't let a real request race a state rebuild still in progress"
// reason as network.NewService's identical rebuildPools call.
func NewService(ctx context.Context, etcdClient *clientv3.Client, identityClient identityv1.TenantServiceClient, computeClient computev1.VirtualMachineServiceClient) (*Service, error) {
	quota, err := newQuotaChecker(ctx)
	if err != nil {
		return nil, err
	}
	svc := &Service{
		volumes: resource.NewStore[Volume, *Volume](etcdClient, "volume", resource.StoreErrors{
			NotFound:      ErrVolumeNotFound,
			Conflict:      ErrVolumeConflict,
			HistoryPruned: ErrVolumeHistoryPruned,
		}),
		attachments: resource.NewStore[VolumeAttachment, *VolumeAttachment](etcdClient, "volattach", resource.StoreErrors{
			NotFound:      ErrVolumeAttachmentNotFound,
			Conflict:      ErrVolumeAttachmentConflict,
			HistoryPruned: ErrVolumeAttachmentHistoryPruned,
		}),
		storageConnections: resource.NewStore[StorageConnection, *StorageConnection](etcdClient, "storageconnection", resource.StoreErrors{
			NotFound:      ErrStorageConnectionNotFound,
			Conflict:      ErrStorageConnectionConflict,
			HistoryPruned: ErrStorageConnectionHistoryPruned,
		}),
		identityClient: identityClient,
		computeClient:  computeClient,
		quota:          quota,
		usage:          make(map[string]tenantUsage),
	}
	if err := svc.rebuildUsage(ctx); err != nil {
		return nil, err
	}
	return svc, nil
}

// rebuildUsage restores usage's in-memory per-tenant quota accounting from
// every existing Volume in etcd. Found 2026-09-13, the same bug class as
// network's vlanPool/ipPool/nextMACOct (see network.Service.rebuildPools):
// usage is purely in-memory, populated only by CreateVolume/DeleteVolume
// calls made within this process's own lifetime, so without this, EVERY
// restart forgets every tenant's real usage and lets CreateVolume approve
// requests a live tenant_usage would have rejected -- not just a
// hypothetical multi-replica problem, though it's that too (each replica's
// own usage map would drift the moment either one creates/deletes a
// Volume, letting a tenant split requests across replicas to bypass
// max_volume_gb entirely).
func (s *Service) rebuildUsage(ctx context.Context) error {
	volumes, err := s.volumes.List(ctx, "")
	if err != nil {
		return fmt.Errorf("block-storage: rebuild usage: list volumes: %w", err)
	}
	usage := make(map[string]tenantUsage)
	for _, vol := range volumes {
		u := usage[vol.Meta.TenantID]
		u.VolumeGB += vol.Spec.SizeGB
		usage[vol.Meta.TenantID] = u
	}
	s.usage = usage
	return nil
}

// Run retries Pending VolumeAttachments (blocked by another active
// attachment on the same volume_id at Create time) every
// pendingSweepInterval until ctx is done, and -- once nc/js are non-nil --
// also drives the StorageConnection/Volume verification flow (see
// verification.go): subscribing to Hypervisor storage-connection reports
// and Volume verify-results, and periodically sweeping both toward Ready.
// Safe to call from only one goroutine; cmd/block-storage/main.go starts it
// once at startup.
func (s *Service) Run(ctx context.Context, nc *nats.Conn, js jetstream.JetStream) error {
	s.nc = nc
	s.js = js

	if js != nil {
		if err := EnsureStreams(ctx, js); err != nil {
			return err
		}
		if err := s.subscribeVerifyResults(ctx); err != nil {
			return fmt.Errorf("subscribe verify results: %w", err)
		}
		if err := s.subscribeVolumeAttached(ctx); err != nil {
			return fmt.Errorf("subscribe volume attached: %w", err)
		}
		// subscribeHypervisorStorageConnections depends on compute's own
		// COMPUTE_EVT stream existing, unlike the two subscriptions just
		// above (which only need block-storage's own BLOCKSTORAGE_EVT
		// stream, already ensured a few lines up) -- a genuine cross-service
		// startup race (compute may not have finished starting, or briefly
		// be down), not a reason to abort every other subscription and the
		// sweep loop below if it doesn't resolve quickly. Runs its own
		// indefinitely-retrying goroutine instead of blocking here (real
		// bug found and fixed 2026-09-12: this used to be a synchronous,
		// bounded-retry call in this same sequence, so exhausting its
		// retries here made Run() return early and silently skipped
		// subscribeVerifyResults/subscribeVolumeAttached/the sweep loop
		// entirely, for this process's whole lifetime).
		go s.runHypervisorStorageConnectionsSubscription(ctx)
	}

	go s.watchPendingVolumeAttachments(ctx)
	if js != nil {
		go s.watchPendingVolumes(ctx)
	}

	ticker := time.NewTicker(pendingSweepInterval)
	defer ticker.Stop()
	orphanTicker := time.NewTicker(orphanSweepInterval)
	defer orphanTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.retryPendingAttachments(ctx)
			s.sweepStorageConnections(ctx)
			s.sweepPendingVolumes(ctx)
		case <-orphanTicker.C:
			s.sweepOrphanedVolumeAttachments(ctx)
		}
	}
}

// watchPendingVolumeAttachments attempts the exclusive-attach check
// (tryAttach) immediately when a VolumeAttachment is created while still
// Pending, instead of waiting up to pendingSweepInterval for the periodic
// sweep to notice it -- mirrors network.Service.watchPendingSubnets'
// identical "the common, successful case shouldn't have to wait for a
// sweep tick" reasoning. Deliberately reacts to EventAdded only, not
// EventModified: tryAttach's own Update call on a blocked attempt is
// itself a Modified event, and reacting to that too would retry as fast as
// etcd round-trips complete instead of waiting for the old attachment to
// actually clear -- retryPendingAttachments' periodic sweep is the right
// (and sufficient) backstop for that case.
func (s *Service) watchPendingVolumeAttachments(ctx context.Context) {
	events, err := s.WatchVolumeAttachments(ctx, "", 0)
	if err != nil {
		slog.Error("watch volume attachments for pending attach failed", "err", err)
		return
	}
	for e := range events {
		if e.Type != EventAdded || e.Object.Status.Phase != VolumeAttachmentPhasePending {
			continue
		}
		a := e.Object
		s.tryAttach(ctx, &a)
	}
}

// watchPendingVolumes mirrors watchPendingVolumeAttachments, for verifying
// a newly-created Volume immediately instead of waiting for
// sweepPendingVolumes' next tick -- see CreateVolume's doc comment for why
// this moved here rather than being called directly from Create. Only
// started when js is non-nil (i.e. only in cmd/block-storage-reconciler);
// verifyVolume itself would just no-op without it anyway (see its own
// nil-check), but there's nothing to gain from running this loop in a
// process that can never act on what it finds.
func (s *Service) watchPendingVolumes(ctx context.Context) {
	events, err := s.WatchVolumes(ctx, "", 0)
	if err != nil {
		slog.Error("watch volumes for pending verification failed", "err", err)
		return
	}
	for e := range events {
		if e.Type != EventAdded || e.Object.Status.Phase != VolumePhasePending {
			continue
		}
		if s.verifyEligibleAndAdvance(e.Object.Meta.ID) {
			s.verifyVolume(ctx, e.Object)
		}
	}
}

// sweepOrphanedVolumeAttachments implements docs/architecture.md's
// orphan-GC detection logic (a child checks its own parent's existence via
// Get, deletes itself if NotFound) for VolumeAttachment -- see
// orphanSweepInterval's doc comment for why this exists at all. Any error
// other than NotFound (compute unreachable, a transient RPC failure) is
// treated as "don't know, so don't delete" and just retried next tick --
// only a definitive NotFound is evidence of real orphaning.
func (s *Service) sweepOrphanedVolumeAttachments(ctx context.Context) {
	if s.computeClient == nil {
		return // e.g. in tests that never set one
	}
	attachments, err := s.attachments.List(ctx, "")
	if err != nil {
		return
	}
	for _, att := range attachments {
		_, err := s.computeClient.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: att.Meta.TenantID, Id: att.Spec.VMID})
		if err == nil {
			continue
		}
		if status.Code(err) != codes.NotFound {
			slog.Warn("orphan sweep: could not confirm VolumeAttachment's VM status, skipping this tick", "attachment_id", att.Meta.ID, "vm_id", att.Spec.VMID, "err", err)
			continue
		}
		if derr := s.DeleteVolumeAttachment(ctx, att.Meta.TenantID, att.Meta.ID); derr != nil {
			slog.Error("orphan sweep: delete orphaned VolumeAttachment failed", "attachment_id", att.Meta.ID, "vm_id", att.Spec.VMID, "err", derr)
			continue
		}
		slog.Info("orphan sweep: deleted VolumeAttachment whose VM no longer exists", "attachment_id", att.Meta.ID, "vm_id", att.Spec.VMID)
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
// ErrQuotaExceeded if it would be exceeded, then registers the reference
// (no external backend call -- see the package doc comment: kyuusha never
// provisions anything).
//
// The new Volume starts Pending, not Ready: see verification.go for the
// asynchronous flow (via NATS, never blocking this call) that promotes it
// to Ready once its StorageConnection is itself Ready and its own
// identifier has been confirmed to actually exist. Never Error -- an
// unverified Volume just stays Pending (docs/open-questions.md「Volumeの
// 申告内容...」).
func (s *Service) CreateVolume(ctx context.Context, tenantID, name string, spec VolumeSpec) (*Volume, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if spec.SizeGB <= 0 {
		return nil, fmt.Errorf("%w: spec.size_gb must be positive", ErrValidation)
	}
	switch spec.Protocol {
	case StorageProtocolISCSI, StorageProtocolNVMeOF, StorageProtocolNFS:
	default:
		return nil, fmt.Errorf("%w: spec.protocol must be ISCSI, NVME_OF, or NFS", ErrValidation)
	}
	if spec.StorageConnection == "" {
		return nil, fmt.Errorf("%w: spec.storage_connection is required", ErrValidation)
	}
	if spec.Identifier == "" {
		return nil, fmt.Errorf("%w: spec.identifier is required", ErrValidation)
	}
	if _, ok := s.storageConnections.LookupByName(ctx, "", spec.StorageConnection); !ok {
		return nil, fmt.Errorf("%w: storage_connection %q does not exist", ErrValidation, spec.StorageConnection)
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if existing, ok := s.volumes.LookupByName(ctx, tenantID, name); ok {
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

	out, err := s.volumes.Create(ctx, tenantID, name, Volume{
		Spec:   spec,
		Status: VolumeStatus{Phase: VolumePhasePending},
	})
	if err != nil {
		return nil, err
	}

	usage.VolumeGB += spec.SizeGB
	s.usage[tenantID] = usage

	// verifyVolume itself is never called here -- it needs s.js, which is
	// only ever non-nil in cmd/block-storage-reconciler (the API binary
	// never sets it, so this handler must not touch it directly, the same
	// "don't run process-local/replica-unsafe side effects from the API
	// path" reasoning as network.Service.CreateSubnet). The immediate
	// attempt watchPendingVolumes makes on this Volume's own Added event
	// is this same "don't make the common case wait a full sweep interval"
	// optimization, just relocated to the reconciler.
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

// DeleteVolume rejects a Volume still referenced by a non-Deleting/non-Error
// VolumeAttachment, then deletes the local record and releases the
// Volume's size_gb from tenant_usage in the same critical section,
// mirroring compute's identical Delete. No external backend call -- see the
// package doc comment.
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

// CreateStorageConnection is idempotent when name is set, same shape as
// CreateVolume. Not tenant-scoped (same as Hypervisor) -- always stored
// under tenant_id "". Starts Pending; see verification.go for how it
// reaches Ready.
func (s *Service) CreateStorageConnection(ctx context.Context, name string, spec StorageConnectionSpec) (*StorageConnection, error) {
	if len(spec.Zones) == 0 {
		return nil, fmt.Errorf("%w: spec.zones must have at least one zone", ErrValidation)
	}
	if existing, ok := s.storageConnections.LookupByName(ctx, "", name); ok {
		return &existing, nil
	}
	out, err := s.storageConnections.Create(ctx, "", name, StorageConnection{
		Spec:   spec,
		Status: StorageConnectionStatus{Phase: StorageConnectionPhasePending},
	})
	if err != nil {
		return nil, err
	}
	// Best-effort immediate recompute, in case Hypervisors already
	// reported this connection's name before it existed as a resource here
	// (verification.go's sweep would otherwise only pick it up on the next
	// tick).
	s.sweepStorageConnections(ctx)
	out, getErr := s.storageConnections.Get(ctx, "", out.Meta.ID)
	if getErr != nil {
		return nil, getErr
	}
	return &out, nil
}

func (s *Service) GetStorageConnection(ctx context.Context, id string) (*StorageConnection, error) {
	out, err := s.storageConnections.Get(ctx, "", id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListStorageConnections(ctx context.Context) ([]StorageConnection, error) {
	return s.storageConnections.List(ctx, "")
}

// DeleteStorageConnection rejects a StorageConnection still referenced by
// any Volume (by name -- see Volume.Spec.StorageConnection), the same
// "never leave a referencing resource dangling" rule DeleteVolume applies
// to VolumeAttachment.
func (s *Service) DeleteStorageConnection(ctx context.Context, id string) error {
	sc, err := s.storageConnections.Get(ctx, "", id)
	if err != nil {
		return err
	}
	vols, err := s.volumes.List(ctx, "")
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Spec.StorageConnection == sc.Meta.Name {
			return fmt.Errorf("%w: storage_connection %q still referenced by volume %q", ErrValidation, sc.Meta.Name, v.Meta.ID)
		}
	}
	return s.storageConnections.Delete(ctx, "", id)
}

func (s *Service) WatchStorageConnections(ctx context.Context, sinceRV int64) (<-chan StorageConnectionEvent, error) {
	return s.storageConnections.Watch(ctx, "", sinceRV, nil)
}

// CreateVolumeAttachment validates spec.volume_id against an existing,
// Ready Volume in the same tenant (same "never create a resource that
// references something that can't back it" rule as network's
// CreateNetworkInterface), then enforces the exclusive-attach constraint
// from docs/architecture.md "具体的な排他制御": if another non-Deleting/
// non-Error VolumeAttachment already holds this volume_id, the new one is
// created Pending with a WaitingForOldAttachmentRelease Condition instead
// of being rejected -- the old one may well be in the middle of a normal
// Detach, not stuck -- and Run's periodic sweep retries it once that
// clears.
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

	if existing, ok := s.attachments.LookupByName(ctx, tenantID, name); ok {
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

	// Always created Pending -- the actual exclusive-attach attempt
	// (tryAttach, guarded by attachMu) happens only in
	// cmd/block-storage-reconciler (see watchPendingVolumeAttachments/
	// retryPendingAttachments), never here. See docs/architecture.md
	// "コントロールプレーンサービス自体の可用性": attachMu only serializes
	// within a single process, so this API handler must never call
	// tryAttach directly -- doing so would make it unsafe to run more than
	// one replica of this binary (two replicas could each see "not
	// blocked" and both attach the same volume_id).
	out, err := s.attachments.Create(ctx, tenantID, name, VolumeAttachment{
		Spec:   spec,
		Status: VolumeAttachmentStatus{Phase: VolumeAttachmentPhasePending},
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// tryAttach mutates a in place: Attached on success (no other non-Deleting/
// non-Error VolumeAttachment currently holds a.Spec.VolumeID), or still
// Pending with a WaitingForOldAttachmentRelease Condition otherwise
// (retried later by Run's sweep). No external backend call: whether the
// Volume's device/file is actually reachable from whichever Hypervisor the
// VM lands on is discovered by compute-agent itself at boot time (see
// internal/compute-agent/iscsi), not verified here.
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

	a.Status.Phase = VolumeAttachmentPhaseAttached
	a.Status.Conditions = upsertCondition(a.Status.Conditions, resource.Condition{
		Type: "WaitingForOldAttachmentRelease", Status: resource.ConditionFalse, LastTransitionAt: time.Now(),
	})
	if updated, err := s.attachments.Update(ctx, *a); err == nil {
		*a = updated
	}
}

// hasActiveAttachment reports whether some VolumeAttachment other than
// excludeID already holds volumeID in a non-Deleting/non-Error phase -- the
// "at most one active attachment per volume_id" constraint. Scans every
// tenant's attachments (this Service, unlike network's per-Subnet IP
// pools, has no natural per-volume index to key off of): fine at this
// system's target scale (a modest number of live attachments, not VM-scale
// numbers).
func (s *Service) hasActiveAttachment(ctx context.Context, volumeID, excludeID string) (bool, error) {
	all, err := s.attachments.List(ctx, "")
	if err != nil {
		return false, err
	}
	for _, a := range all {
		if a.Meta.ID == excludeID || a.Spec.VolumeID != volumeID {
			continue
		}
		// Deleting: already releasing, doesn't hold the volume. Error:
		// kept for symmetry with the Deleting exclusion (nothing currently
		// sets Error-phase, since tryAttach no longer has an external call
		// that can fail, but excluding it costs nothing and avoids a
		// future Error-setting change silently reintroducing the
		// permanently-stuck-volume bug this exclusion originally fixed).
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

// DeleteVolumeAttachment removes the local record. No external backend
// call: there is nothing to unexport (see the package doc comment).
func (s *Service) DeleteVolumeAttachment(ctx context.Context, tenantID, id string) error {
	if _, err := s.attachments.Get(ctx, tenantID, id); err != nil {
		return err
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
