package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// TestRunWatchLoopResumesAndRecoversFromPrune: a watch that ends is
// re-established from the last resource_version seen (bookmarks
// included, but never handed to handle), and a pruned resume point
// triggers onPruned plus a full replay from 0.
func TestRunWatchLoopResumesAndRecoversFromPrune(t *testing.T) {
	old := watchRetryDelay
	watchRetryDelay = time.Millisecond
	defer func() { watchRetryDelay = old }()

	pruned := errors.New("pruned")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls []int64
	watch := func(_ context.Context, rv int64) (<-chan resource.Event[int], error) {
		calls = append(calls, rv)
		ch := make(chan resource.Event[int], 2)
		switch len(calls) {
		case 1: // first stream: one event and a bookmark, then it drops
			ch <- resource.Event[int]{Type: resource.EventAdded, Object: 1, ResourceVersion: 10}
			ch <- resource.Event[int]{Type: resource.EventBookmark, ResourceVersion: 15}
		case 2: // resume point has been compacted away
			return nil, pruned
		case 3: // full replay
			ch <- resource.Event[int]{Type: resource.EventDeleted, Object: 2, ResourceVersion: 20}
		default:
			cancel()
		}
		close(ch)
		return ch, nil
	}
	var handled []int
	prunedCalls := 0
	runWatchLoop(ctx, "test", watch, pruned, func(context.Context) { prunedCalls++ }, func(e resource.Event[int]) {
		handled = append(handled, e.Object)
	})

	if len(calls) < 4 || calls[0] != 0 || calls[1] != 15 || calls[2] != 0 || calls[3] != 20 {
		t.Fatalf("watch called with resource versions %v, want [0 15 0 20 ...]", calls)
	}
	if prunedCalls != 1 {
		t.Fatalf("onPruned called %d times, want 1", prunedCalls)
	}
	if len(handled) != 2 || handled[0] != 1 || handled[1] != 2 {
		t.Fatalf("handled %v, want [1 2] (no bookmark)", handled)
	}
}
