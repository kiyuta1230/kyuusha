// Package resourcetest is the shared etcd fixture every resource.Store-
// backed service's tests use (compute, identity, image, network,
// block-storage, and resource itself). Starting a fresh embedded etcd
// server per test would be needlessly slow -- server startup alone takes
// several seconds -- so this package starts exactly one embedded instance
// for the whole test binary process (sync.Once) and gives each test its own
// isolated key prefix via go.etcd.io/etcd/client/v3/namespace, rather than
// resetting or restarting anything between tests.
package resourcetest

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/namespace"
	"go.etcd.io/etcd/server/v3/embed"
)

var (
	once     sync.Once
	endpoint string
	startErr error

	prefixCounter atomic.Int64
)

// Client returns a *clientv3.Client whose every key is transparently
// confined to a prefix unique to this call (see namespace.NewKV/NewWatcher
// below) -- concurrent tests, and repeated calls within the same test,
// never see each other's data, even though they all dial the same real
// embedded etcd server under the hood. Each call opens its own connection
// (clientv3.Client embeds sync/atomic-guarded fields that must not be
// struct-copied, so this can't just clone one shared *clientv3.Client) --
// fine for test-process lifetimes; callers don't need to Close it.
//
// testing.TB (not *testing.T) so a *testing.B can use the same fixture --
// needed by any Benchmark* that wants a real etcd-backed Service rather
// than reimplementing this setup.
func Client(t testing.TB) *clientv3.Client {
	t.Helper()
	once.Do(func() {
		endpoint, startErr = startEmbedded()
	})
	if startErr != nil {
		t.Fatalf("resourcetest: start embedded etcd: %v", startErr)
	}

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("resourcetest: dial embedded etcd: %v", err)
	}

	prefix := fmt.Sprintf("/resourcetest/%d/", prefixCounter.Add(1))
	client.KV = namespace.NewKV(client.KV, prefix)
	client.Watcher = namespace.NewWatcher(client.Watcher, prefix)
	return client
}

func startEmbedded() (string, error) {
	sweepStaleDataDirs()

	dir, err := os.MkdirTemp("", "kyuusha-resourcetest-etcd-*")
	if err != nil {
		return "", err
	}

	cfg := embed.NewConfig()
	cfg.Dir = dir
	cfg.Logger = "zap"
	cfg.LogLevel = "error"

	peerURL, err := url.Parse("http://127.0.0.1:0")
	if err != nil {
		return "", err
	}
	clientURL, err := url.Parse("http://127.0.0.1:0")
	if err != nil {
		return "", err
	}
	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.ListenClientUrls = []url.URL{*clientURL}
	cfg.AdvertisePeerUrls = cfg.ListenPeerUrls
	cfg.AdvertiseClientUrls = cfg.ListenClientUrls
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)

	e, err := embed.StartEtcd(cfg)
	if err != nil {
		return "", err
	}

	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(30 * time.Second):
		e.Server.Stop()
		return "", fmt.Errorf("resourcetest: embedded etcd server took too long to start")
	}

	return e.Clients[0].Addr().String(), nil
}

// staleDataDirAge is how old a leftover kyuusha-resourcetest-etcd-* directory
// must be before sweepStaleDataDirs treats it as abandoned rather than
// belonging to a concurrently-running sibling package's test binary. `go
// test ./...` runs multiple packages' test binaries in parallel, each
// calling startEmbedded independently, so a blind "remove everything
// matching the glob" on startup would race a sibling process that just
// created its own directory moments ago. Ten minutes is generous slack
// above this whole test suite's actual runtime (seconds, not minutes) while
// still being short enough to reclaim space within the same working
// session, not just eventually.
const staleDataDirAge = 10 * time.Minute

// sweepStaleDataDirs removes this package's own leftover etcd data
// directories from previous test runs. There is no reliable hook in Go's
// testing package for "run this once when the whole binary is about to
// exit" (TestMain would work, but only if added to every one of the six
// packages that call Client -- compute, identity, image, network,
// block-storage, resource -- not to this shared helper), so startEmbedded
// itself never removes the directory it creates. Left unaddressed, every
// `go test` invocation in a long working session leaks another ~120MB into
// /tmp indefinitely -- this was discovered when it had accumulated to
// several GB and started causing real "no space left on device" failures.
// Best-effort: a removal failure (e.g. this really is a live sibling's
// directory, somehow older than staleDataDirAge on an unusually slow
// machine) is silently skipped, not fatal -- worst case, that one
// directory just isn't reclaimed this time either.
func sweepStaleDataDirs() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleDataDirAge)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "kyuusha-resourcetest-etcd-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(os.TempDir(), e.Name()))
	}
}
