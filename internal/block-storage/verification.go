// This file implements the async StorageConnection/Volume verification
// flow from docs/open-questions.md「Hypervisor↔ストレージバックエンドの接続
// 確立をkyuusha側で自動化すべきか」「具体的な設計」: two independent layers,
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
	"fmt"
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

// conditionSizeMatchesDeclaration is set alongside conditionIdentifierVerified
// from the same round-trip's real observed size (VerifyVolumeResult.SizeBytes,
// computed by internal/compute-agent/volumeref.Resolve) -- see
// docs/open-questions.md「Volumeの申告内容...」. Never re-checked afterward
// (the verification round-trip itself only ever runs once per Volume, same
// as identifier existence -- see docs/open-questions.md's static-vs-dynamic
// entry): if the real backing store is resized later, this condition goes
// stale, same as everything else that flow confirms once.
const conditionSizeMatchesDeclaration = "SizeMatchesDeclaration"

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

// hypervisorConnInfo is one Hypervisor's self-report, as last seen via
// EvtSubjectHypervisorStorageConnections.
type hypervisorConnInfo struct {
	zone        string
	connections map[string]bool // connection name -> present
}

// subscribeHypervisorStorageConnections attaches a durable consumer to
// compute's own COMPUTE_EVT stream (owned/created by compute, not here --
// hence the short retry loop: block-storage and compute may start in
// either order under docker-compose). Never touches compute's gRPC surface.
func (s *Service) subscribeHypervisorStorageConnections(ctx context.Context) error {
	var stream jetstream.Stream
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		stream, err = s.js.Stream(ctx, "COMPUTE_EVT")
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("wait for COMPUTE_EVT stream: %w", err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "block-storage-hypervisor-storage-connections",
		FilterSubject: "ms.compute.evt.*.storage-connections",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return err
	}
	_, err = cons.Consume(func(msg jetstream.Msg) {
		_ = msg.Ack()
		var m compute.HypervisorStorageConnectionsMsg
		if jsonErr := json.Unmarshal(msg.Data(), &m); jsonErr != nil {
			slog.Warn("block-storage: bad hypervisor storage-connections event", "err", jsonErr)
			return
		}
		s.recordHypervisorConnections(ctx, m.Hypervisor, m.Zone, m.StorageConnections)
	})
	return err
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

// sweepPendingVolumes promotes a Pending Volume to Ready once both (1) its
// StorageConnection is Ready and (2) its own identifier has already been
// confirmed (conditionIdentifierVerified); otherwise, if not yet confirmed,
// (re-)sends a verify command -- unconditionally on every tick, not just
// once, since there's no harm in asking again and it's the only way to
// recover from a dropped command/reply.
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
		if !hasCondition(vol.Status.Conditions, conditionIdentifierVerified, resource.ConditionTrue) {
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
	}
	vol.Status.Conditions = upsertCondition(vol.Status.Conditions, resource.Condition{
		Type: conditionIdentifierVerified, Status: status, Reason: reason, LastTransitionAt: time.Now(),
	})

	// The same round-trip that confirms the identifier exists also reports
	// its real observed size (only meaningful on Success -- a failed lookup
	// has no size to compare). vol.Spec.SizeGB<=0 can't happen for a real
	// Volume (Create-time validation), but the check is here as a divide-
	// safety guard, not a real-world case.
	if res.Success && res.SizeBytes > 0 && vol.Spec.SizeGB > 0 {
		sizeStatus := resource.ConditionFalse
		sizeReason := "observed size does not match declared size_gb"
		if sizeMatchesDeclaration(vol.Spec.SizeGB, res.SizeBytes) {
			sizeStatus = resource.ConditionTrue
			sizeReason = ""
		}
		vol.Status.Conditions = upsertCondition(vol.Status.Conditions, resource.Condition{
			Type: conditionSizeMatchesDeclaration, Status: sizeStatus, Reason: sizeReason,
			Message:          fmt.Sprintf("declared %d GB, observed %d bytes", vol.Spec.SizeGB, res.SizeBytes),
			LastTransitionAt: time.Now(),
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
