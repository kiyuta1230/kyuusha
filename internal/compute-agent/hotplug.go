package computeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/volumeref"
)

// hotplugAgentTimeout bounds how long serveHotplug waits for the underlying
// driver call (a real api-socket round trip) before giving up -- kept
// shorter than the Reconciler's own hotplugTimeout (internal/compute/
// liveops.go) so the caller's timeout fires because of a genuinely slow/
// unreachable driver, not a race against this handler's own deadline.
const hotplugAgentTimeout = 8 * time.Second

// handleHotplug is this hypervisor's COMPUTE_CMD consumer callback for live
// Resize/AttachVolume/DetachVolume (see compute.CmdSubjectHotplug). Acks on
// receipt (same at-least-once/ack-on-receipt contract as handleStop), then
// hands off to a goroutine: a real api-socket call can take a moment, and
// must not block this consumer from accepting its next delivery.
func (a *Agent) handleHotplug(msg jetstream.Msg) {
	_ = msg.Ack()

	var cmd compute.HotplugCommand
	if err := json.Unmarshal(msg.Data(), &cmd); err != nil {
		slog.Error("compute-agent: bad hotplug command", "err", err)
		return
	}
	go a.serveHotplug(cmd)
}

func (a *Agent) serveHotplug(cmd compute.HotplugCommand) {
	ctx, cancel := context.WithTimeout(context.Background(), hotplugAgentTimeout)
	defer cancel()

	driver, ok := a.Drivers[cmd.DriverHint]
	if !ok {
		a.publishHotplugResult(cmd.ReplySubject, fmt.Errorf("no driver registered for driver_hint %q", cmd.DriverHint))
		return
	}
	hp, ok := driver.(vmm.Hotplugger)
	if !ok {
		a.publishHotplugResult(cmd.ReplySubject, fmt.Errorf("driver_hint %q does not support live hotplug", cmd.DriverHint))
		return
	}

	switch cmd.Op {
	case compute.HotplugOpResize:
		a.publishHotplugResult(cmd.ReplySubject, hp.LiveResize(ctx, cmd.VMID, cmd.VCPU, cmd.MemoryMB))
	case compute.HotplugOpAddDisk:
		a.serveHotplugAddDisk(ctx, cmd, hp)
	case compute.HotplugOpRemoveDevice:
		a.publishHotplugResult(cmd.ReplySubject, hp.LiveRemoveDevice(ctx, cmd.VMID, cmd.DeviceID))
	default:
		a.publishHotplugResult(cmd.ReplySubject, fmt.Errorf("unknown hotplug op %q", cmd.Op))
	}
}

// serveHotplugAddDisk resolves the Volume's real device/file path (the same
// discovery a real VM boot does -- see buildVolumeInfos/volumeref.Resolve)
// and hotplugs it, then reports the result via reportAttachedVolumes -- the
// exact same block-storage report-back path a boot-time attach already
// uses, so status.device_path/status.hypervisor gets populated identically
// either way.
func (a *Agent) serveHotplugAddDisk(ctx context.Context, cmd compute.HotplugCommand, hp vmm.Hotplugger) {
	devPath, sizeBytes, err := volumeref.Resolve(a.LocalStorageConnections, cmd.Protocol, cmd.StorageConnection, cmd.Identifier)
	if err != nil {
		a.publishHotplugResult(cmd.ReplySubject, err)
		return
	}
	if err := hp.LiveAddDisk(ctx, cmd.VMID, cmd.DeviceID, devPath); err != nil {
		a.publishHotplugResult(cmd.ReplySubject, err)
		return
	}
	vmm.WarnIfSizeMismatch(vmm.VolumeAttachInfo{AttachmentID: cmd.DeviceID, SizeGB: cmd.SizeGB}, sizeBytes)
	a.reportAttachedVolumes(ctx, []vmm.AttachedVolume{{AttachmentID: cmd.DeviceID, TenantID: cmd.TenantID, DevicePath: devPath}})
	a.publishHotplugResult(cmd.ReplySubject, nil)
}

// publishHotplugResult replies on replySubject via plain core NATS, not
// COMPUTE_EVT -- see compute.HotplugResult's doc comment: only the one
// blocked RPC caller (Reconciler.roundTripHotplug) ever reads this.
func (a *Agent) publishHotplugResult(replySubject string, opErr error) {
	res := compute.HotplugResult{Success: opErr == nil}
	if opErr != nil {
		res.Error = opErr.Error()
	}
	payload, err := json.Marshal(res)
	if err != nil {
		slog.Error("compute-agent: encode hotplug result failed", "err", err)
		return
	}
	if err := a.NC.Publish(replySubject, payload); err != nil {
		slog.Warn("compute-agent: publish hotplug result failed", "reply_subject", replySubject, "err", err)
	}
}
