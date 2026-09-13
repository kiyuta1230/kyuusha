// Command block-storage runs the block-storage control-plane's gRPC API:
// VolumeService, VolumeAttachmentService, and StorageConnectionService.
// Every handler is a direct etcd read/write via blockstorage.Service --
// CreateVolumeAttachment leaves every VolumeAttachment Pending, never
// attempting the exclusive-attach check itself (see its doc comment), so
// this binary touches neither attachMu nor NATS/JetStream at all (no
// -nats-url, no -compute-addr: the orphan-GC sweep, the only user of a
// compute client, lives entirely in the reconciler) and is safe to run as
// any number of replicas behind a load balancer.
//
// All actual reconciliation (exclusive-attach attempts, the
// StorageConnection/Volume verification flow, retry/orphan sweeps) lives
// in the separate cmd/block-storage-reconciler binary instead -- see its
// own package doc comment for the single-replica deployment invariant
// that binary depends on (2026-09-13, mirroring cmd/compute-reconciler's
// and cmd/network-reconciler's identical splits: docs/architecture.md
// "コントロールプレーンサービス自体の可用性" "Reconcile面"). See also
// docs/specs/volume.md. kyuusha doesn't provision or export storage itself
// (see docs/architecture.md「訂正: 責務の境界を...」) -- there is no
// dedicated storage-node service to dial; Volume/VolumeAttachment/
// StorageConnection are pure reference metadata.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	blockstorage "github.com/kiyuta1230/kyuusha/internal/block-storage"
	"github.com/kiyuta1230/kyuusha/internal/block-storage/grpcserver"
	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8085", "address to serve VolumeService/VolumeAttachmentService/StorageConnectionService on")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	metricsAddr := flag.String("metrics-addr", ":9097", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers and used when dialing other services (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA both callers' and dialed services' certificates must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "block-storage", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("block-storage")
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

	etcdClient, err := etcdconn.Connect(*etcdEndpoints)
	if err != nil {
		slog.Error("connect to etcd", "endpoints", *etcdEndpoints, "err", err)
		os.Exit(1)
	}
	defer etcdClient.Close()

	// computeClient is nil: this binary's Service is never passed to Run,
	// so the orphan-GC sweep (its only user) never runs here anyway -- see
	// this package's doc comment.
	svc, err := blockstorage.NewService(ctx, etcdClient, identityv1.NewTenantServiceClient(identityConn), nil)
	if err != nil {
		slog.Error("new block-storage service", "err", err)
		os.Exit(1)
	}

	serverCreds, err := mtls.ServerCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		slog.Error("load mTLS server credentials", "err", err)
		os.Exit(1)
	}

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		slog.Error("listen", "addr", *grpcAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(grpc.Creds(serverCreds), grpc.StatsHandler(otelgrpc.NewServerHandler()))
	blockstoragev1.RegisterVolumeServiceServer(grpcServer, grpcserver.NewVolumeServer(svc))
	blockstoragev1.RegisterVolumeAttachmentServiceServer(grpcServer, grpcserver.NewVolumeAttachmentServer(svc))
	blockstoragev1.RegisterStorageConnectionServiceServer(grpcServer, grpcserver.NewStorageConnectionServer(svc))

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("block-storage: serving VolumeService/VolumeAttachmentService", "addr", *grpcAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
