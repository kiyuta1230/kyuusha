package network

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// watchRetryDelay is how long runWatchLoop waits before re-establishing a
// watch that ended. Package var so tests can shrink it.
var watchRetryDelay = time.Second

// runWatchLoop keeps handle fed with every event of a cross-tenant watch
// until ctx is done. A store watch ends on its own whenever the underlying
// etcd watch does (leader change, network blip); without this loop the
// caller would silently stop allocating -- and stop returning VLAN IDs/IPs
// on Deleted events -- until the process restarts. It resumes from the
// last resource_version seen (bookmarks included), so changes made while
// disconnected, deletions among them, are replayed rather than lost. If
// that point has already been compacted away (pruned), it calls onPruned
// and starts over from a full replay.
func runWatchLoop[T any](ctx context.Context, name string, watch func(context.Context, int64) (<-chan resource.Event[T], error), pruned error, onPruned func(context.Context), handle func(resource.Event[T])) {
	var sinceRV int64
	for {
		events, err := watch(ctx, sinceRV)
		switch {
		case err == nil:
			for e := range events {
				sinceRV = e.ResourceVersion
				if e.Type != resource.EventBookmark {
					handle(e)
				}
			}
		case errors.Is(err, pruned):
			slog.Warn("network: watch resume point compacted away, replaying from scratch", "watch", name, "since_rv", sinceRV)
			onPruned(ctx)
			sinceRV = 0
			continue
		default:
			slog.Warn("network: watch failed", "watch", name, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchRetryDelay):
		}
	}
}
