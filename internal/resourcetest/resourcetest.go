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
func Client(t *testing.T) *clientv3.Client {
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
