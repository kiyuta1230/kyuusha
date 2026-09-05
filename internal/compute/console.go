package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// consoleFirstResponseTimeout bounds how long StreamConsole waits for
// compute-agent's very first reply (an immediate ack, real data, or a done/
// error marker) before giving up and reporting the hypervisor unreachable.
// compute-agent always sends something right away (see console.go's
// serveConsole), so this is not a "did the guest finish booting" timeout --
// only "is anyone home".
const consoleFirstResponseTimeout = 5 * time.Second

// StreamConsole asks vmID's Hypervisor (via its compute-agent, over NATS --
// see docs/specs/firecracker-boot.md) for its serial console output, and
// relays the reply onto the returned channel until the request completes
// (follow=false and history exhausted), the caller's ctx is done, or
// compute-agent gives up (its own follow safety timeout). The channel is
// always closed exactly once, whether by success, error, or cancellation.
func (r *Reconciler) StreamConsole(ctx context.Context, tenantID, vmID string, tailBytes int64, follow bool) (<-chan []byte, error) {
	vm, err := r.svc.Get(ctx, tenantID, vmID)
	if err != nil {
		return nil, err
	}
	if vm.Status.Hypervisor == "" {
		return nil, fmt.Errorf("%w: vm has no assigned hypervisor yet (never scheduled)", ErrValidation)
	}

	replySubject := r.nc.NewInbox()
	sub, err := r.nc.SubscribeSync(replySubject)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(ConsoleRequest{
		VMID:         vmID,
		TailBytes:    tailBytes,
		Follow:       follow,
		ReplySubject: replySubject,
	})
	if err != nil {
		sub.Unsubscribe()
		return nil, err
	}
	if err := r.nc.Publish(ConsoleRequestSubject(vm.Status.Hypervisor), payload); err != nil {
		sub.Unsubscribe()
		return nil, err
	}

	firstCtx, cancel := context.WithTimeout(ctx, consoleFirstResponseTimeout)
	first, err := sub.NextMsgWithContext(firstCtx)
	cancel()
	if err != nil {
		sub.Unsubscribe()
		return nil, fmt.Errorf("no response from hypervisor %q's compute-agent (is it running?): %w", vm.Status.Hypervisor, err)
	}
	if errMsg := first.Header.Get(ConsoleErrorHeader); errMsg != "" {
		sub.Unsubscribe()
		return nil, fmt.Errorf("%w: %s", ErrValidation, errMsg)
	}

	out := make(chan []byte, 16)
	stopSubject := replySubject + ".stop"

	// relay forwards one message's payload (if non-empty and not a done
	// marker) onto out. It returns false when msg is terminal (done marker,
	// or the caller's ctx is done while trying to send) -- the caller
	// should stop reading from the subscription in that case.
	relay := func(msg *nats.Msg) bool {
		if msg.Header.Get(ConsoleDoneHeader) != "" {
			return false
		}
		if len(msg.Data) == 0 {
			return true
		}
		select {
		case out <- msg.Data:
			return true
		case <-ctx.Done():
			return false
		}
	}

	go func() {
		defer close(out)
		defer sub.Unsubscribe()
		if !relay(first) {
			if ctx.Err() != nil {
				_ = r.nc.Publish(stopSubject, nil)
			}
			return
		}
		for {
			msg, err := sub.NextMsgWithContext(ctx)
			if err != nil {
				_ = r.nc.Publish(stopSubject, nil) // best-effort; harmless if compute-agent already stopped
				return
			}
			if !relay(msg) {
				if ctx.Err() != nil {
					_ = r.nc.Publish(stopSubject, nil)
				}
				return
			}
		}
	}()

	return out, nil
}
