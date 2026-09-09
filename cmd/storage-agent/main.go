// Command storage-agent runs the StorageBackendService gRPC API for one
// storage node: a real ZFS pool exported over iSCSI (LIO/targetcli). See
// internal/storage-agent and docs/specs/volume.md. internal/block-storage
// is the only client, at a single static address -- no registration or
// multi-node scheduling, matching v1's "1台〜数台" scale
// (docs/architecture.md).
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

	"gitlab.com/ki.yuta1230/kyuusha/internal/mtls"
	storageagent "gitlab.com/ki.yuta1230/kyuusha/internal/storage-agent"
	"gitlab.com/ki.yuta1230/kyuusha/internal/storage-agent/grpcserver"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	storageagentv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/storageagent/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8092", "address to serve StorageBackendService on")
	metricsAddr := flag.String("metrics-addr", ":9098", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA callers' certificates must chain to")
	zpoolName := flag.String("zpool-name", "kyuusha-tank", "ZFS pool name Volumes are created in (as zvols)")
	zpoolBackingFile := flag.String("zpool-backing-file", "/var/lib/kyuusha/zpool-backing.img", "sparse file backing the pool, created on first startup if the pool doesn't already exist. Must be a path that resolves to the same file from the *host's* mount namespace too -- see internal/storage-agent's package doc comment for why (zpool create genuinely fails from inside a container's own mount namespace) and playground/docker-compose.yml for the identical-source/target bind-mount this depends on")
	zpoolSizeGB := flag.Int64("zpool-size-gb", 50, "size of the backing file created for a fresh pool (ignored if the pool already exists)")
	portalHost := flag.String("portal-host", "localhost:3260", "host:port other containers/hosts should dial to reach this node's iSCSI portal. Its port is also the port LIO actually binds the network portal on for every target -- see internal/storage-agent's ExportVolume")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "storage-agent", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("storage-agent")
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

	backend := &storageagent.Backend{ZpoolName: *zpoolName, PortalHost: *portalHost}
	if err := backend.EnsurePool(ctx, *zpoolBackingFile, *zpoolSizeGB); err != nil {
		slog.Error("ensure zpool", "err", err)
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
	storageagentv1.RegisterStorageBackendServiceServer(grpcServer, grpcserver.NewServer(backend))

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("storage-agent: serving StorageBackendService", "addr", *grpcAddr, "zpool", *zpoolName, "portal", *portalHost)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
