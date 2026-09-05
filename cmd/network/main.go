// Command network runs the network control-plane: the SubnetService and
// NetworkInterfaceService gRPC APIs. See docs/architecture.md "networkサービス
// のリソース: Subnet / NetworkInterface" and docs/specs/network.md. This first
// pass is CRUD+Watch only, with Create going straight to Ready using mocked
// allocation (no real IPAM, no tap wiring, no agent side at all yet).
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

	"gitlab.com/ki.yuta1230/kyuusha/internal/network"
	"gitlab.com/ki.yuta1230/kyuusha/internal/network/grpcserver"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	networkv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8084", "address to serve SubnetService/NetworkInterfaceService on")
	metricsAddr := flag.String("metrics-addr", ":9096", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "network", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("network")
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

	svc := network.NewService()

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		slog.Error("listen", "addr", *grpcAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	networkv1.RegisterSubnetServiceServer(grpcServer, grpcserver.NewSubnetServer(svc))
	networkv1.RegisterNetworkInterfaceServiceServer(grpcServer, grpcserver.NewNetworkInterfaceServer(svc))

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("network: serving SubnetService/NetworkInterfaceService", "addr", *grpcAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
