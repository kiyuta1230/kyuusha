package blockstorage

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Subject naming mirrors internal/compute/nats.go's convention:
// ms.<service>.<cmd|evt>.<hypervisor>.<resource-type>.<verb>

// CmdSubjectVerifyVolume/EvtSubjectVerifyVolumeResult are the command/event
// pair block-storage's Service and compute-agent exchange directly over
// NATS to confirm a Volume's identifier actually exists (and get its real
// size) on some Hypervisor holding its storage_connection -- see
// docs/open-questions.md「Hypervisor↔ストレージバックエンドの接続確立を
// kyuusha側で自動化すべきか」「具体的な設計」. Deliberately NOT routed through
// compute (which already depends on block-storage for Volume validation --
// routing this the other way would make that dependency bidirectional).
func CmdSubjectVerifyVolume(hypervisor string) string {
	return fmt.Sprintf("ms.blockstorage.cmd.%s.volume.verify", hypervisor)
}

func EvtSubjectVerifyVolumeResult(hypervisor string) string {
	return fmt.Sprintf("ms.blockstorage.evt.%s.volume.verify-result", hypervisor)
}

// VerifyVolumeCommand is published to CmdSubjectVerifyVolume(hypervisor)
// for one specific, already-chosen Hypervisor (see Service.verifyVolume) --
// everything compute-agent's internal/compute-agent/volumeref.Resolve needs
// travels here, the same "no separate service client, everything needed
// travels in the message" shape compute's own CreateCommand uses.
type VerifyVolumeCommand struct {
	// TenantID travels here (and back on VerifyVolumeResult) purely so
	// Service can do a normal tenant-scoped Get when the result comes back
	// -- compute-agent never interprets it, just echoes it.
	TenantID          string `json:"tenant_id"`
	VolumeID          string `json:"volume_id"`
	Protocol          string `json:"protocol"`
	StorageConnection string `json:"storage_connection"`
	Identifier        string `json:"identifier"`
}

// VerifyVolumeResult is compute-agent's reply, published to
// EvtSubjectVerifyVolumeResult(hypervisor) (the same hypervisor the command
// was sent to -- block-storage's consumer filters on it, mirroring
// compute-agent's own per-hypervisor CreateResult consumer).
type VerifyVolumeResult struct {
	TenantID  string `json:"tenant_id"`
	VolumeID  string `json:"volume_id"`
	Success   bool   `json:"success"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Error     string `json:"error,omitempty"`
}

const (
	cmdStreamName = "BLOCKSTORAGE_CMD"
	evtStreamName = "BLOCKSTORAGE_EVT"
)

// EnsureStreams creates BLOCKSTORAGE_CMD/BLOCKSTORAGE_EVT if they don't
// already exist. Safe to call from both block-storage and compute-agent at
// startup (mirrors compute.EnsureStreams).
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cmdStreamName,
		Subjects:  []string{"ms.blockstorage.cmd.>"},
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		return fmt.Errorf("ensure %s stream: %w", cmdStreamName, err)
	}

	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      evtStreamName,
		Subjects:  []string{"ms.blockstorage.evt.>"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("ensure %s stream: %w", evtStreamName, err)
	}
	return nil
}
