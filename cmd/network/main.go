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

	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/network/grpcserver"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8084", "address to serve SubnetService/NetworkInterfaceService on")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address, for the orphaned-NetworkInterface sweep (does this NetworkInterface's vm_id still exist?)")
	metricsAddr := flag.String("metrics-addr", ":9096", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers and used when dialing compute (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA both callers' and compute's certificates must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
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

	etcdClient, err := etcdconn.Connect(*etcdEndpoints)
	if err != nil {
		slog.Error("connect to etcd", "endpoints", *etcdEndpoints, "err", err)
		os.Exit(1)
	}
	defer etcdClient.Close()

	clientCreds, err := mtls.ClientCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		slog.Error("load mTLS client credentials", "err", err)
		os.Exit(1)
	}
	computeConn, err := grpc.NewClient(*computeAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()

	svc, err := network.NewService(ctx, etcdClient, computev1.NewVirtualMachineServiceClient(computeConn))
	if err != nil {
		slog.Error("new network service", "err", err)
		os.Exit(1)
	}
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
