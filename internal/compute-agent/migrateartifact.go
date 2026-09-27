package computeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/imagestore"
)

// pushRootDiskTimeout bounds how long servePushRootDisk spends reading the
// root disk file and pushing it -- generous, since this is a real file
// read + network upload proportional to disk size, not a quick call (kept
// shorter than the Reconciler's own pushRootDiskTimeout, internal/compute/
// liveops.go, so the caller's timeout fires because of a genuinely slow/
// unreachable registry, not a race against this handler's own deadline).
const pushRootDiskTimeout = 4 * time.Minute

// deleteMigrationArtifactTimeout is much shorter: a manifest delete is one
// small registry API call, not a file transfer.
const deleteMigrationArtifactTimeout = 15 * time.Second

// handleMigrateArtifact is this hypervisor's COMPUTE_CMD consumer callback
// for Migrate(transfer_root_disk=true)'s push and cleanup delete (see
// compute.CmdSubjectMigrateArtifact). Acks on receipt (same at-least-once/
// ack-on-receipt contract as handleHotplug), then hands off to a goroutine:
// both operations can take a moment and must not block this consumer from
// accepting its next delivery.
func (a *Agent) handleMigrateArtifact(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.MigrateArtifactCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad migrate-artifact command", "err", err)
		return
	}
	switch cmd.Op {
	case compute.MigrateArtifactOpPush:
		go a.servePushRootDisk(cmd)
	case compute.MigrateArtifactOpDelete:
		go a.serveDeleteMigrationArtifact(cmd)
	default:
		slog.Error("compute-agent: unknown migrate-artifact op", "op", cmd.Op)
	}
}

func (a *Agent) servePushRootDisk(cmd compute.MigrateArtifactCommand) {
	ctx, cancel := context.WithTimeout(context.Background(), pushRootDiskTimeout)
	defer cancel()

	if a.MigrationRegistry == "" {
		a.publishMigrateArtifactResult(cmd.ReplySubject, compute.MigrateArtifactResult{}, fmt.Errorf("this compute-agent has no -migration-registry configured, root disk transfer is unavailable on this host"))
		return
	}
	driver, ok := a.Drivers[cmd.DriverHint]
	if !ok || driver == nil {
		a.publishMigrateArtifactResult(cmd.ReplySubject, compute.MigrateArtifactResult{}, fmt.Errorf("no driver registered for driver_hint %q", cmd.DriverHint))
		return
	}
	path, err := driver.RootDiskPath(cmd.VMID)
	if err != nil {
		a.publishMigrateArtifactResult(cmd.ReplySubject, compute.MigrateArtifactResult{}, err)
		return
	}

	// One tag per push (never reused): this VM may be migrated again
	// later, and each push must land at its own distinct reference so an
	// in-flight pull of an earlier migration's artifact can never be
	// clobbered by a newer one under the same tag.
	tag := fmt.Sprintf("migrate-%s-%d", cmd.VMID, time.Now().UnixNano())
	url, digest, err := imagestore.PushOCIBlob(ctx, a.MigrationRegistry, a.MigrationRegistryRef, "kyuusha-migration/"+cmd.VMID, tag, a.MigrationRegistryPlainHTTP, path)
	if err != nil {
		a.publishMigrateArtifactResult(cmd.ReplySubject, compute.MigrateArtifactResult{}, fmt.Errorf("push root disk to migration registry: %w", err))
		return
	}
	a.publishMigrateArtifactResult(cmd.ReplySubject, compute.MigrateArtifactResult{URL: url, Digest: digest}, nil)
}

func (a *Agent) serveDeleteMigrationArtifact(cmd compute.MigrateArtifactCommand) {
	ctx, cancel := context.WithTimeout(context.Background(), deleteMigrationArtifactTimeout)
	defer cancel()

	ref, plainHTTP, err := parseMigrationArtifactURL(cmd.URL)
	if err != nil {
		slog.Warn("compute-agent: dropping malformed migration artifact URL, nothing to delete", "url", cmd.URL, "err", err)
		return
	}
	// Best-effort by design (see imagestore.DeleteOCIRef's doc comment):
	// no reply, no retry. A leaked artifact costs registry storage, never
	// correctness.
	if err := imagestore.DeleteOCIRef(ctx, ref, plainHTTP); err != nil {
		slog.Warn("compute-agent: delete migration artifact failed (registry storage will leak this one)", "url", cmd.URL, "err", err)
	}
}

// parseMigrationArtifactURL strips PushOCIBlob's own "oci://"/"oci+http://"
// scheme prefix back off a URL it returned, the same scheme convention
// internal/compute-agent/imagestore.open dispatches on for a normal Image
// pull.
func parseMigrationArtifactURL(url string) (ref string, plainHTTP bool, err error) {
	if ref, ok := strings.CutPrefix(url, "oci+http://"); ok {
		return ref, true, nil
	}
	if ref, ok := strings.CutPrefix(url, "oci://"); ok {
		return ref, false, nil
	}
	return "", false, fmt.Errorf("unrecognized scheme in %q, want oci:// or oci+http://", url)
}

// publishMigrateArtifactResult replies on replySubject via plain core NATS,
// not COMPUTE_EVT -- see compute.MigrateArtifactResult's doc comment: only
// the one blocked RPC caller (Reconciler.roundTripPushRootDisk) ever reads
// this. A no-op if replySubject is empty (DELETE commands have none).
func (a *Agent) publishMigrateArtifactResult(replySubject string, res compute.MigrateArtifactResult, opErr error) {
	if replySubject == "" {
		return
	}
	res.Success = opErr == nil
	if opErr != nil {
		res.Error = opErr.Error()
	}
	payload, err := json.Marshal(res)
	if err != nil {
		slog.Error("compute-agent: encode migrate-artifact result failed", "err", err)
		return
	}
	if err := a.NC.Publish(replySubject, payload); err != nil {
		slog.Warn("compute-agent: publish migrate-artifact result failed", "reply_subject", replySubject, "err", err)
	}
}
