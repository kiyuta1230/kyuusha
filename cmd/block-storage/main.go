// Command block-storage runs the block-storage control-plane: the
// VolumeService and VolumeAttachmentService gRPC APIs, plus the periodic
// sweep that retries VolumeAttachments left Pending by the exclusive-attach
// constraint. See docs/architecture.md "block-storageサービスのリソース:
// Volume / VolumeAttachment" and docs/specs/volume.md. There is no real
// StorageBackend yet -- see docs/architecture.md
// "block-storageのバックエンド抽象化" for the eventual ZFS/NVMe-oF design.
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
	"google.golang.org/grpc/credentials/insecure"

	blockstorage "gitlab.com/ki.yuta1230/kyuusha/internal/block-storage"
	"gitlab.com/ki.yuta1230/kyuusha/internal/block-storage/grpcserver"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8085", "address to serve VolumeService/VolumeAttachmentService on")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	metricsAddr := flag.String("metrics-addr", ":9097", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
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

	// block-storage -> identity is plaintext for now; see api-gateway's
	// identical note on the mTLS follow-up.
	identityConn, err := grpc.NewClient(*identityAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial identity", "addr", *identityAddr, "err", err)
		os.Exit(1)
	}
	defer identityConn.Close()

	svc, err := blockstorage.NewService(ctx, identityv1.NewTenantServiceClient(identityConn))
	if err != nil {
		slog.Error("new block-storage service", "err", err)
		os.Exit(1)
	}
	go func() {
		if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("pending sweep stopped", "err", err)
		}
	}()

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		slog.Error("listen", "addr", *grpcAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	blockstoragev1.RegisterVolumeServiceServer(grpcServer, grpcserver.NewVolumeServer(svc))
	blockstoragev1.RegisterVolumeAttachmentServiceServer(grpcServer, grpcserver.NewVolumeAttachmentServer(svc))

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
