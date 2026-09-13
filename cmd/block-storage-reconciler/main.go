// Command block-storage-reconciler runs only block-storage's reconcile
// loop (blockstorage.Service.Run): exclusive-attach attempts for every
// VolumeAttachment CreateVolumeAttachment leaves Pending, the
// StorageConnection/Volume verification flow over NATS (see
// internal/block-storage/verification.go and
// docs/open-questions.md「Hypervisor↔ストレージバックエンドの接続確立を
// kyuusha側で自動化すべきか」), and the retry/orphan sweeps. It serves no
// gRPC API at all -- cmd/block-storage is the VolumeService/
// VolumeAttachmentService/StorageConnectionService gRPC binary, safely run
// as any number of stateless replicas (see its own package doc comment).
//
// Deployment invariant this binary depends on: run exactly one replica of
// block-storage-reconciler at a time -- see cmd/compute-reconciler's and
// cmd/network-reconciler's identical invariant and doc comment for the
// full reasoning (docs/architecture.md "コントロールプレーンサービス自体の
// 可用性" "Reconcile面"). Two replicas running simultaneously could both
// see a volume_id as "not blocked" and both attach it (attachMu only
// serializes within a single process), and would each maintain their own
// independent tenant quota usage that drifts the moment either one
// creates/deletes a Volume.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	blockstorage "github.com/kiyuta1230/kyuusha/internal/block-storage"
	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL, for the StorageConnection/Volume verification flow (see internal/block-storage/verification.go)")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address, for the orphaned-VolumeAttachment sweep (does this VolumeAttachment's vm_id still exist?)")
	metricsAddr := flag.String("metrics-addr", ":9100", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate used when dialing other services (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA dialed services' certificates must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "block-storage-reconciler", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("block-storage-reconciler")
	if err != nil {
		slog.Error("setup metrics", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownMetrics(shutdownCtx)
	}()
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metricsHandler)
		if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
			slog.Error("metrics server stopped", "err", err)
		}
	}()

	clientCreds, err := mtls.ClientCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		slog.Error("load mTLS client credentials", "err", err)
		os.Exit(1)
	}

	identityConn, err := grpc.NewClient(*identityAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial identity", "addr", *identityAddr, "err", err)
		os.Exit(1)
	}
	defer identityConn.Close()

	computeConn, err := grpc.NewClient(*computeAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()

	nc, err := nats.Connect(*natsURL)
	if err != nil {
		slog.Error("connect to nats", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		slog.Error("create jetstream context", "err", err)
		os.Exit(1)
	}

	etcdClient, err := etcdconn.Connect(*etcdEndpoints)
	if err != nil {
		slog.Error("connect to etcd", "endpoints", *etcdEndpoints, "err", err)
		os.Exit(1)
	}
	defer etcdClient.Close()

	// A second, independent blockstorage.Service instance from the API
	// binary's own -- both need one (Run's methods read/write through it),
	// but neither shares process memory (including attachMu/usage) with
	// the other, only the etcd/NATS state both actually connect to.
	svc, err := blockstorage.NewService(ctx, etcdClient, identityv1.NewTenantServiceClient(identityConn), computev1.NewVirtualMachineServiceClient(computeConn))
	if err != nil {
		slog.Error("new block-storage service", "err", err)
		os.Exit(1)
	}

	slog.Info("block-storage-reconciler: starting")
	if err := svc.Run(ctx, nc, js); err != nil && ctx.Err() == nil {
		slog.Error("reconciler stopped", "err", err)
		os.Exit(1)
	}
}
