// This file implements the async StorageConnection/Volume verification
// flow from docs/specs/volume.md「検証フロー」: two independent layers,
// both driven entirely over NATS (never a new gRPC dependency on compute --
// see nats.go's doc comments for why), both strict-but-patient (a Volume or
// StorageConnection that never gets confirmed just stays Pending forever,
// never Error).
//
//  1. Connection-level (recordHypervisorConnections/sweepStorageConnections):
//     shared by every Volume using a given StorageConnection. Just
//     correlates Hypervisor self-reports (relayed here by compute's
//     Reconciler, see internal/compute/nats.go's
//     EvtSubjectHypervisorStorageConnections) against each
//     StorageConnection's own spec.zones.
//  2. Volume-level (verifyVolume/handleVerifyResult): one round-trip per
//     Volume, to whichever Hypervisor currently reports holding its
//     storage_connection, actually confirming the specific identifier
//     exists (and its real size -- compute-agent's own
//     internal/compute-agent/volumeref.Resolve already computes this).
package blockstorage

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// conditionIdentifierVerified is the Volume.Status.Conditions type set once
// verifyVolume's round-trip confirms the identifier exists -- Volume phase
// promotion (sweepPendingVolumes) checks this alongside the referenced
// StorageConnection's own phase.
const conditionIdentifierVerified = "IdentifierVerified"

// conditionSizeMatchesDeclaration reflects whether spec.size_gb currently
// matches the real observed size (VerifyVolumeResult.SizeBytes, computed by
// internal/compute-agent/volumeref.Resolve) -- see docs/open-questions.md
// 「Volumeの申告内容...」. A mismatch beyond sizeMismatchTolerance is
// corrected in place (see correctDeclaredSize) rather than just flagged: a
// storage admin mistyping size_gb is common and easy, and there is no value
// in leaving Quota accounting wrong indefinitely once the real size is
// known -- see that function's own doc comment for why this is safe given
// kyuusha's declarative spec/status model. So in practice this condition is
// True immediately after any correction; a stale False would only be
// visible if the Update racing the correction itself failed.
const conditionSizeMatchesDeclaration = "SizeMatchesDeclaration"

// conditionQuotaExceededAfterCorrection is set True when correctDeclaredSize
// finds the corrected size pushes the tenant over max_volume_gb. Purely
// informational -- correction still applies regardless (see its own doc
// comment): kyuusha doesn't retroactively destroy or detach an
// already-existing, possibly already-attached Volume over a Quota
// realization discovered after the fact.
const conditionQuotaExceededAfterCorrection = "QuotaExceededAfterCorrection"

// bytesPerGB/sizeMismatchTolerance mirror
// internal/compute-agent/vmm.WarnIfSizeMismatch's own constants exactly, but
// are kept independent rather than shared: that package boots VMs and logs a
// warning, this one is a separate service recording a Condition, and the two
// have no other reason to depend on each other.
const (
	bytesPerGB            = 1 << 30
	sizeMismatchTolerance = 0.10
)

// sizeMatchesDeclaration reports whether observedBytes is within
// sizeMismatchTolerance of declaredSizeGB (loose enough to absorb ordinary
// backend rounding/overhead, tight enough to catch a size_gb that's wildly
// wrong) -- see vmm.WarnIfSizeMismatch's identical reasoning.
func sizeMatchesDeclaration(declaredSizeGB, observedBytes int64) bool {
	declaredBytes := declaredSizeGB * bytesPerGB
	diff := observedBytes - declaredBytes
	if diff < 0 {
		diff = -diff
	}
	return float64(diff) <= float64(declaredBytes)*sizeMismatchTolerance
}

// ceilBytesToGB rounds up, matching the usual cloud/storage convention of
// never under-billing a partial GB.
func ceilBytesToGB(b int64) int64 {
	return (b + bytesPerGB - 1) / bytesPerGB
}

// correctDeclaredSize brings vol.Spec.SizeGB and this tenant's tracked
// tenant_usage.VolumeGB in line with observedBytes when they've drifted
// beyond sizeMismatchTolerance, and reports (via
// conditionQuotaExceededAfterCorrection) if the corrected total now exceeds
// the tenant's max_volume_gb. Mutates vol in place; the caller is
// responsible for the actual Update.
//
// Why correcting is safe here, unlike most places this codebase is careful
// never to retroactively act on a Quota realization: size_gb is
// Create-time-only accounting metadata a storage admin has to type by
// hand (kyuusha never provisions, so there's no other source for it --
// see docs/architecture.md「訂正: 責務の境界を...」), and typos are common
// and easy. Once the real size is known there is no value in leaving the
// declaration (and the Quota accounting derived from it) wrong forever --
// this is exactly the kind of "spec was a best-effort declaration, status
// now reflects reality" reconciliation this whole verification flow (and
// kyuusha's object model generally) already embodies elsewhere. Unlike a
// genuine over-quota Create rejection, this never destroys or blocks an
// already-existing (possibly already-attached) Volume -- see
// conditionQuotaExceededAfterCorrection's own doc comment.
func (s *Service) correctDeclaredSize(ctx context.Context, vol *Volume, observedBytes int64) {
	if vol.Spec.SizeGB <= 0 || observedBytes <= 0 || sizeMatchesDeclaration(vol.Spec.SizeGB, observedBytes) {
		return
	}

	oldSizeGB := vol.Spec.SizeGB
	correctedSizeGB := ceilBytesToGB(observedBytes)
	vol.Spec.SizeGB = correctedSizeGB

	s.usageMu.Lock()
	usage := s.usage[vol.Meta.TenantID]
	usage.VolumeGB += correctedSizeGB - oldSizeGB
	s.usage[vol.Meta.TenantID] = usage
	s.usageMu.Unlock()

	slog.Info("block-storage: corrected volume's declared size_gb to its real observed size",
		"id", vol.Meta.ID, "declared_gb", oldSizeGB, "corrected_gb", correctedSizeGB)

	exceeded := resource.ConditionFalse
	if limit, err := lookupQuota(ctx, s.identityClient, vol.Meta.TenantID); err == nil {
		// usage.VolumeGB already includes this volume's own corrected share
		// (just added above), so "usage minus that share, plus that share
		// again" run through the same allow() the rest of Quota enforcement
		// uses is exactly "is the corrected total within max_volume_gb" --
		// reusing it rather than a hand-rolled comparison, per
		// docs/architecture.md's own "Quota設計" instruction.
		if allowed, err := s.quota.allow(ctx, tenantUsage{VolumeGB: usage.VolumeGB - correctedSizeGB}, correctedSizeGB, limit); err == nil && !allowed {
			exceeded = resource.ConditionTrue
			slog.Warn("block-storage: volume's corrected size exceeds tenant quota", "id", vol.Meta.ID, "tenant_id", vol.Meta.TenantID, "corrected_gb", correctedSizeGB, "usage_gb", usage.VolumeGB)
		}
	}
	vol.Status.Conditions = upsertCondition(vol.Status.Conditions, resource.Condition{
		Type: conditionQuotaExceededAfterCorrection, Status: exceeded, LastTransitionAt: time.Now(),
	})
}

// hypervisorConnInfo is one Hypervisor's self-report, as last seen via
// EvtSubjectHypervisorStorageConnections.
type hypervisorConnInfo struct {
	zone        string
	connections map[string]bool // connection name -> present
}

// runHypervisorStorageConnectionsSubscription attaches a durable consumer to
// compute's own COMPUTE_EVT stream (owned/created by compute, not here).
// Never touches compute's gRPC surface. Blocks, retrying indefinitely (not a
// bounded attempt count) until it manages to subscribe, or ctx is done --
// see its caller (Run) for why this must never give up permanently:
// compute's COMPUTE_EVT stream not existing yet (or compute being briefly
// down) is an ordinary startup race (block-storage and compute may start in
// either order under docker-compose), not a fatal condition for the rest of
// block-storage. Logs a warning on each failed attempt so a genuinely stuck
// wait is still visible in the logs, without treating it as fatal.
func (s *Service) runHypervisorStorageConnectionsSubscription(ctx context.Context) {
	var stream jetstream.Stream
	for {
		var err error
		stream, err = s.js.Stream(ctx, "COMPUTE_EVT")
		if err == nil {
			break
		}
		slog.Warn("block-storage: waiting for compute's COMPUTE_EVT stream", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "block-storage-hypervisor-storage-connections",
		FilterSubject: "ms.compute.evt.*.storage-connections",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		slog.Error("block-storage: create hypervisor storage-connections consumer failed", "err", err)
		return
	}
	if _, err := cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var m compute.HypervisorStorageConnectionsMsg
		if jsonErr := json.Unmarshal(msg.Data(), &m); jsonErr != nil {
			slog.Warn("block-storage: bad hypervisor storage-connections event", "err", jsonErr)
			return
		}
		s.recordHypervisorConnections(ctx, m.Hypervisor, m.Zone, m.StorageConnections)
	}); err != nil {
		slog.Error("block-storage: consume hypervisor storage-connections failed", "err", err)
	}
}

// subscribeVerifyResults attaches a durable consumer to block-storage's own
// BLOCKSTORAGE_EVT stream (already ensured by EnsureStreams before this is
// called -- see Run).
func (s *Service) subscribeVerifyResults(ctx context.Context) error {
	stream, err := s.js.Stream(ctx, evtStreamName)
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "block-storage-verify-results",
		FilterSubject: "ms.blockstorage.evt.*.volume.verify-result",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	_, err = cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var res VerifyVolumeResult
		if jsonErr := json.Unmarshal(msg.Data(), &res); jsonErr != nil {
			slog.Warn("block-storage: bad verify-volume-result event", "err", jsonErr)
			return
		}
		s.handleVerifyResult(ctx, res)
	})
	return err
}

// subscribeVolumeAttached mirrors subscribeVerifyResults exactly, for
// compute-agent's separate, proactive "here's where a Volume actually
// landed" report -- see nats.go's EvtSubjectVolumeAttached doc comment and
// docs/specs/volume.md "status.device_path/status.hypervisor".
func (s *Service) subscribeVolumeAttached(ctx context.Context) error {
	stream, err := s.js.Stream(ctx, evtStreamName)
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "block-storage-volume-attached",
		FilterSubject: "ms.blockstorage.evt.*.volume.attached",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	_, err = cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var ev VolumeAttachedEvent
		if jsonErr := json.Unmarshal(msg.Data(), &ev); jsonErr != nil {
			slog.Warn("block-storage: bad volume-attached event", "err", jsonErr)
			return
		}
		s.handleVolumeAttached(ctx, ev)
	})
	return err
}

// handleVolumeAttached records where a VolumeAttachment actually landed.
// Purely informational: it never changes Phase (the exclusive-attach
// bookkeeping in service.go's tryAttach already owns that -- see its own
// doc comment) and silently no-ops if the VolumeAttachment is gone by the
// time this arrives (deleted, or from some now-superseded earlier attach).
func (s *Service) handleVolumeAttached(ctx context.Context, ev VolumeAttachedEvent) {
	att, err := s.attachments.Get(ctx, ev.TenantID, ev.AttachmentID)
	if err != nil {
		return
	}
	att.Status.DevicePath = ev.DevicePath
	att.Status.Hypervisor = ev.Hypervisor
	if _, err := s.attachments.Update(ctx, att); err != nil {
		slog.Warn("block-storage: record volume-attached report failed", "attachment_id", ev.AttachmentID, "err", err)
	}
}

// recordHypervisorConnections updates this Service's in-memory view of
// hypervisor -> (zone, connection names) and immediately re-sweeps
// StorageConnections, so a fresh Hypervisor report can promote one to Ready
// right away instead of waiting a full pendingSweepInterval.
func (s *Service) recordHypervisorConnections(ctx context.Context, hypervisor, zone string, connectionNames []string) {
	connSet := make(map[string]bool, len(connectionNames))
	for _, c := range connectionNames {
		connSet[c] = true
	}
	s.hcMu.Lock()
	if s.hypervisorConnections == nil {
		s.hypervisorConnections = make(map[string]hypervisorConnInfo)
	}
	s.hypervisorConnections[hypervisor] = hypervisorConnInfo{zone: zone, connections: connSet}
	s.hcMu.Unlock()

	s.sweepStorageConnections(ctx)
}

// sweepStorageConnections recomputes verified_zones/phase for every
// non-Ready StorageConnection from the current hypervisorConnections view.
// Strict: phase only becomes Ready once every spec.zones entry is covered.
func (s *Service) sweepStorageConnections(ctx context.Context) {
	all, err := s.storageConnections.List(ctx, "")
	if err != nil {
		return
	}

	s.hcMu.Lock()
	hc := make(map[string]hypervisorConnInfo, len(s.hypervisorConnections))
	for k, v := range s.hypervisorConnections {
		hc[k] = v
	}
	s.hcMu.Unlock()

	for _, sc := range all {
		if sc.Status.Phase == StorageConnectionPhaseReady {
			continue
		}
		verifiedSet := map[string]bool{}
		for _, info := range hc {
			if info.connections[sc.Meta.Name] {
				verifiedSet[info.zone] = true
			}
		}
		var verified []string
		for z := range verifiedSet {
			verified = append(verified, z)
		}
		sort.Strings(verified)

		allVerified := true
		for _, z := range sc.Spec.Zones {
			if !verifiedSet[z] {
				allVerified = false
				break
			}
		}

		newPhase := StorageConnectionPhasePending
		if allVerified {
			newPhase = StorageConnectionPhaseReady
		}
		if newPhase == sc.Status.Phase && stringSlicesEqual(sc.Status.VerifiedZones, verified) {
			continue // nothing changed: skip the Update, avoid needless resource_version churn
		}
		sc.Status.Phase = newPhase
		sc.Status.VerifiedZones = verified
		if _, err := s.storageConnections.Update(ctx, sc); err != nil {
			slog.Warn("block-storage: update storage connection verification failed", "id", sc.Meta.ID, "err", err)
		}
	}
}

// verifyRetryInterval/verifyRetryMax tune what used to be an untuned retry
// (docs/specs/volume.md「検証コマンドのリトライ間隔・上限は未チューニング」):
// sweepPendingVolumes used to (re-)send a VerifyVolumeCommand for every
// still-unverified Pending Volume on every single pendingSweepInterval
// tick, forever -- fine for a Volume that resolves within the first few
// ticks, wasteful (unbounded NATS traffic + compute-agent volumeref.Resolve
// calls, forever) for one whose identifier will simply never exist (typo,
// decommissioned backend, etc). verifyEligibleAndAdvance now backs off
// exponentially per-Volume, starting at the same cadence as before and
// capped at verifyRetryMax. Deliberately still has no attempt cap and never
// moves the Volume to Error -- same "確認できなければPendingのまま"
// philosophy as the rest of this file, just slower about re-asking once a
// Volume has gone unverified for a while.
const (
	verifyRetryInterval = pendingSweepInterval
	verifyRetryMax      = 5 * time.Minute
)

// verifyRetryState is verifyBackoff's per-Volume value (Service.
// verifyBackoff, service.go): next is when this Volume becomes eligible for
// another VerifyVolumeCommand, interval is the delay that produced it (so
// the next backoff can double from it).
type verifyRetryState struct {
	next     time.Time
	interval time.Duration
}

// verifyEligibleAndAdvance reports whether it's time to (re-)send a
// VerifyVolumeCommand for volumeID, and if so, atomically schedules the
// next eligible attempt (doubled from last time, capped at verifyRetryMax).
// The schedule is advanced up front, on the attempt itself, not only after
// a failure reply comes back -- a command that never gets a reply at all
// (e.g. no Hypervisor currently reports this storage_connection, so
// verifyVolume never even publishes one) must still back off, not retry
// every tick forever.
func (s *Service) verifyEligibleAndAdvance(volumeID string) bool {
	s.verifyBackoffMu.Lock()
	defer s.verifyBackoffMu.Unlock()
	prev, ok := s.verifyBackoff[volumeID]
	if ok && time.Now().Before(prev.next) {
		return false
	}
	interval := verifyRetryInterval
	if ok {
		interval = prev.interval * 2
		if interval > verifyRetryMax {
			interval = verifyRetryMax
		}
	}
	if s.verifyBackoff == nil {
		s.verifyBackoff = make(map[string]verifyRetryState)
	}
	s.verifyBackoff[volumeID] = verifyRetryState{next: time.Now().Add(interval), interval: interval}
	return true
}

// clearVerifyBackoff drops volumeID's backoff state once its identifier has
// actually been confirmed (handleVerifyResult on success), so a Volume that
// later needs re-verification for any reason starts fresh rather than
// inheriting however far it had backed off before.
func (s *Service) clearVerifyBackoff(volumeID string) {
	s.verifyBackoffMu.Lock()
	defer s.verifyBackoffMu.Unlock()
	delete(s.verifyBackoff, volumeID)
}

// sweepPendingVolumes promotes a Pending Volume to Ready once both (1) its
// StorageConnection is Ready and (2) its own identifier has already been
// confirmed (conditionIdentifierVerified); otherwise, if not yet confirmed
// and not currently backed off (verifyEligibleAndAdvance), (re-)sends a
// verify command -- there's no harm in asking again and it's the only way
// to recover from a dropped command/reply.
func (s *Service) sweepPendingVolumes(ctx context.Context) {
	all, err := s.volumes.List(ctx, "")
	if err != nil {
		return
	}
	for i := range all {
		vol := all[i]
		if vol.Status.Phase != VolumePhasePending {
			continue
		}
		sc, ok := s.storageConnections.LookupByName(ctx, "", vol.Spec.StorageConnection)
		if !ok {
			continue // shouldn't happen post-Create-time validation; nothing to do if it does anyway
		}
		if sc.Status.Phase == StorageConnectionPhaseReady && hasCondition(vol.Status.Conditions, conditionIdentifierVerified, resource.ConditionTrue) {
			vol.Status.Phase = VolumePhaseReady
			if _, err := s.volumes.Update(ctx, vol); err != nil {
				slog.Warn("block-storage: promote volume to Ready failed", "id", vol.Meta.ID, "err", err)
			}
			continue
		}
		if !hasCondition(vol.Status.Conditions, conditionIdentifierVerified, resource.ConditionTrue) && s.verifyEligibleAndAdvance(vol.Meta.ID) {
			s.verifyVolume(ctx, vol)
		}
	}
}

// verifyVolume publishes a VerifyVolumeCommand to one Hypervisor currently
// reporting vol.Spec.StorageConnection (any one -- see nats.go's doc
// comment: existence/size don't vary by which Hypervisor happens to answer,
// only reachability from a given zone does, and that's StorageConnection's
// own separate concern). A no-op if none currently do, or if Run hasn't
// started yet (s.js nil, e.g. in a test) -- the periodic sweep retries
// either way.
func (s *Service) verifyVolume(ctx context.Context, vol Volume) {
	if s.js == nil {
		return
	}
	hypervisor := s.pickHypervisorForConnection(vol.Spec.StorageConnection)
	if hypervisor == "" {
		return
	}
	payload, err := json.Marshal(VerifyVolumeCommand{
		TenantID:          vol.Meta.TenantID,
		VolumeID:          vol.Meta.ID,
		Protocol:          string(vol.Spec.Protocol),
		StorageConnection: vol.Spec.StorageConnection,
		Identifier:        vol.Spec.Identifier,
	})
	if err != nil {
		return
	}
	msg := nats.NewMsg(CmdSubjectVerifyVolume(hypervisor))
	msg.Data = payload
	if _, err := s.js.PublishMsg(ctx, msg); err != nil {
		slog.Warn("block-storage: publish verify-volume command failed", "volume_id", vol.Meta.ID, "hypervisor", hypervisor, "err", err)
	}
}

func (s *Service) pickHypervisorForConnection(connection string) string {
	s.hcMu.Lock()
	defer s.hcMu.Unlock()
	for hv, info := range s.hypervisorConnections {
		if info.connections[connection] {
			return hv
		}
	}
	return ""
}

// handleVerifyResult records compute-agent's answer as a Condition on the
// Volume (not an immediate phase flip -- sweepPendingVolumes/the immediate
// call below still checks the StorageConnection side too before promoting
// to Ready).
func (s *Service) handleVerifyResult(ctx context.Context, res VerifyVolumeResult) {
	vol, err := s.volumes.Get(ctx, res.TenantID, res.VolumeID)
	if err != nil {
		return // Volume deleted since the command was sent, most likely
	}
	status := resource.ConditionFalse
	reason := res.Error
	if res.Success {
		status = resource.ConditionTrue
		reason = ""
		s.clearVerifyBackoff(res.VolumeID)
	}
	vol.Status.Conditions = upsertCondition(vol.Status.Conditions, resource.Condition{
		Type: conditionIdentifierVerified, Status: status, Reason: reason, LastTransitionAt: time.Now(),
	})

	// The same round-trip that confirms the identifier exists also reports
	// its real observed size (only meaningful on Success -- a failed lookup
	// has no size to compare). correctDeclaredSize corrects spec.size_gb (and
	// tenant_usage) in place if it's drifted beyond tolerance -- see its own
	// doc comment for why that's safe here. vol.Spec.SizeGB<=0 can't happen
	// for a real Volume (Create-time validation); correctDeclaredSize guards
	// it anyway, defensively. Once corrected, spec.size_gb equals what was
	// just observed by definition, so this condition is always True
	// immediately afterward -- see its own doc comment.
	if res.Success && res.SizeBytes > 0 {
		s.correctDeclaredSize(ctx, &vol, res.SizeBytes)
		vol.Status.Conditions = upsertCondition(vol.Status.Conditions, resource.Condition{
			Type: conditionSizeMatchesDeclaration, Status: resource.ConditionTrue, LastTransitionAt: time.Now(),
		})
	}

	updated, err := s.volumes.Update(ctx, vol)
	if err != nil {
		return
	}

	sc, ok := s.storageConnections.LookupByName(ctx, "", updated.Spec.StorageConnection)
	if ok && sc.Status.Phase == StorageConnectionPhaseReady && status == resource.ConditionTrue && updated.Status.Phase == VolumePhasePending {
		updated.Status.Phase = VolumePhaseReady
		if _, err := s.volumes.Update(ctx, updated); err != nil {
			slog.Warn("block-storage: promote volume to Ready failed", "id", updated.Meta.ID, "err", err)
		}
	}
}

func hasCondition(conditions []resource.Condition, typ string, want resource.ConditionStatus) bool {
	for _, c := range conditions {
		if c.Type == typ {
			return c.Status == want
		}
	}
	return false
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
