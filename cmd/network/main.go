// Command network runs the network control-plane: the SubnetService and
// NetworkInterfaceService gRPC APIs, plus the periodic sweep that retries
// Subnets/NetworkInterfaces left Pending by pool exhaustion. See
// docs/architecture.md "networkサービスのリソース: Subnet / NetworkInterface" and
// docs/specs/network.md. VLAN ID/IP allocation (IPAM) is real; there is
// still no tap wiring and no agent side at all.
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
	"gitlab.com/ki.yuta1230/kyuusha/internal/network"
	"gitlab.com/ki.yuta1230/kyuusha/internal/network/grpcserver"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	networkv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8084", "address to serve SubnetService/NetworkInterfaceService on")
	metricsAddr := flag.String("metrics-addr", ":9096", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA callers' certificates must chain to")
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
	go func() {
		if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
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
