// Package etcdconn is the shared "-etcd-endpoints flag -> *clientv3.Client"
// boilerplate every kyuusha control-plane service's main.go uses (see
// docs/architecture.md "採用: バッキングストアにetcdを採用する") -- kept in one
// place instead of five near-identical copies.
package etcdconn

import (
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Connect dials etcd at the comma-separated endpoints (e.g. "etcd:2379", or
// "etcd-0:2379,etcd-1:2379,etcd-2:2379" for a real cluster).
func Connect(endpoints string) (*clientv3.Client, error) {
	eps := strings.Split(endpoints, ",")
	for i := range eps {
		eps[i] = strings.TrimSpace(eps[i])
	}
	return clientv3.New(clientv3.Config{
		Endpoints:   eps,
		DialTimeout: 5 * time.Second,
	})
}
