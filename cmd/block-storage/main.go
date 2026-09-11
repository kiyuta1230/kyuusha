// Command block-storage runs the block-storage control-plane: the
// VolumeService, VolumeAttachmentService, and StorageConnectionService gRPC
// APIs, plus the periodic sweep that retries VolumeAttachments left Pending
// by the exclusive-attach constraint, and (over NATS) the StorageConnection/
// Volume verification flow -- see internal/block-storage/verification.go
// and docs/open-questions.md「Hypervisor↔ストレージバックエンドの接続確立を
// kyuusha側で自動化すべきか」. See also docs/architecture.md
// "block-storageサービスのリソース: Volume / VolumeAttachment" and
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

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	blockstorage "gitlab.com/ki.yuta1230/kyuusha/internal/block-storage"
	"gitlab.com/ki.yuta1230/kyuusha/internal/block-storage/grpcserver"
	"gitlab.com/ki.yuta1230/kyuusha/internal/mtls"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8085", "address to serve VolumeService/VolumeAttachmentService/StorageConnectionService on")
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL, for the StorageConnection/Volume verification flow (see internal/block-storage/verification.go)")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	metricsAddr := flag.String("metrics-addr", ":9097", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers and used when dialing other services (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA both callers' and dialed services' certificates must chain to")
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

	svc, err := blockstorage.NewService(ctx, identityv1.NewTenantServiceClient(identityConn))
	if err != nil {
		slog.Error("new block-storage service", "err", err)
		os.Exit(1)
	}
	go func() {
		if err := svc.Run(ctx, nc, js); err != nil && ctx.Err() == nil {
			slog.Error("pending sweep stopped", "err", err)
		}
	}()

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
