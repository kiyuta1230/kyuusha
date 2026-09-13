// Command compute-reconciler runs only compute's Reconciler: the
// Watch-driven scheduling loop, the Pending/stuck-phase retry sweep, the
// Hypervisor health sweep, and everything else that talks to compute-agent
// over NATS (see internal/compute/reconciler.go). It serves no gRPC API at
// all -- cmd/compute is the VirtualMachineService/HypervisorService gRPC
// binary, safely run as any number of stateless replicas behind a load
// balancer (see its own package doc comment).
//
// Deployment invariant this binary depends on: run exactly one replica of
// compute-reconciler at a time. Its reconcile loop has no leader-election
// guard (see docs/architecture.md "コントロールプレーンサービス自体の可用性"
// "Reconcile面") -- two replicas running simultaneously would both try to
// schedule/retry the same VirtualMachines, the same class of duplicate
// work (not corruption, thanks to resource_version optimistic concurrency,
// but wasteful and noisy) that section describes. This is a deliberate,
// simpler alternative to etcd-based leader election: splitting the
// reconcile loop into its own single-instance process, restarted by
// whatever orchestrator runs it (a brief reconcile gap on a crash, instead
// of a hot standby taking over instantly) rather than kept perpetually
// hot-standby-replicated. compute (the API binary) needs no such
// invariant -- it's genuinely stateless and safe at any replica count.
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
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	imageAddr := flag.String("image-addr", "localhost:8083", "image service address, for resolving a VM's Image at provisioning time")
	networkAddr := flag.String("network-addr", "localhost:8084", "network service address, for creating a scheduled VM's NetworkInterfaces")
	blockStorageAddr := flag.String("block-storage-addr", "localhost:8085", "block-storage service address, for creating a scheduled VM's VolumeAttachments")
	metricsAddr := flag.String("metrics-addr", ":9098", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate used when dialing other services (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA dialed services' certificates must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "compute-reconciler", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("compute-reconciler")
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

	etcdClient, err := etcdconn.Connect(*etcdEndpoints)
	if err != nil {
		slog.Error("connect to etcd", "endpoints", *etcdEndpoints, "err", err)
		os.Exit(1)
	}
	defer etcdClient.Close()

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

	imageConn, err := grpc.NewClient(*imageAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial image", "addr", *imageAddr, "err", err)
		os.Exit(1)
	}
	defer imageConn.Close()

	networkConn, err := grpc.NewClient(*networkAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial network", "addr", *networkAddr, "err", err)
		os.Exit(1)
	}
	defer networkConn.Close()

	blockStorageConn, err := grpc.NewClient(*blockStorageAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial block-storage", "addr", *blockStorageAddr, "err", err)
		os.Exit(1)
	}
	defer blockStorageConn.Close()

	// A second, independent compute.Service instance from the API binary's
	// own -- both need one (Reconciler's methods read/write through
	// r.svc.*), but neither shares process memory with the other, only the
	// etcd/NATS/east-west state both actually connect to.
	svc, err := compute.NewService(ctx, etcdClient,
		identityv1.NewTenantServiceClient(identityConn),
		imagev1.NewImageServiceClient(imageConn),
		networkv1.NewSubnetServiceClient(networkConn),
		networkv1.NewNetworkInterfaceServiceClient(networkConn),
		blockstoragev1.NewVolumeServiceClient(blockStorageConn),
		blockstoragev1.NewVolumeAttachmentServiceClient(blockStorageConn),
	)
	if err != nil {
		slog.Error("new compute service", "err", err)
		os.Exit(1)
	}
	prometheus.MustRegister(compute.NewMetricsCollector(svc))

	slog.Info("compute-reconciler: starting")
	recon := compute.NewReconciler(svc, nc, js)
	if err := recon.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("reconciler stopped", "err", err)
		os.Exit(1)
	}
}
