package compute

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Subject naming follows docs/architecture.md's "NATS JetStream:
// subject/stream設計" section: ms.<service>.<cmd|evt>.<hypervisor>.<resource-type>.<verb>

// CmdSubjectCreate etc. are shared by the compute service (Reconciler) and
// compute-agent, which is why they live here rather than unexported.

func CmdSubjectCreate(hypervisor string) string {
	return fmt.Sprintf("ms.compute.cmd.%s.vm.create", hypervisor)
}

func EvtSubjectCreateResult(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.vm.create-result", hypervisor)
}

func EvtSubjectHeartbeat(hypervisor string) string {
	return fmt.Sprintf("ms.compute.evt.%s.heartbeat", hypervisor)
}

const (
	cmdStreamName = "COMPUTE_CMD"
	evtStreamName = "COMPUTE_EVT"
)

// CreateCommand is published to cmdSubjectCreate(hypervisor) when a VM enters
// Provisioning. compute-agent acks on receipt, not on completion (see
// docs/architecture.md), and reports back on evtSubjectCreateResult.
type CreateCommand struct {
	VMID     string `json:"vm_id"`
	TenantID string `json:"tenant_id"`
	ImageID  string `json:"image_id"`
	VCPU     int32  `json:"vcpu"`
	MemoryMB int64  `json:"memory_mb"`
}

type CreateResult struct {
	VMID    string `json:"vm_id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type HeartbeatMsg struct {
	Hypervisor string    `json:"hypervisor"`
	At         time.Time `json:"at"`
}

// EnsureStreams creates COMPUTE_CMD/COMPUTE_EVT if they don't already exist.
// Safe to call from both compute and compute-agent at startup.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cmdStreamName,
		Subjects:  []string{"ms.compute.cmd.>"},
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		return fmt.Errorf("ensure %s stream: %w", cmdStreamName, err)
	}

	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      evtStreamName,
		Subjects:  []string{"ms.compute.evt.>"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("ensure %s stream: %w", evtStreamName, err)
	}
	return nil
}
