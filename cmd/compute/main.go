// Command compute runs the compute control-plane: the VirtualMachineService
// and HypervisorService gRPC APIs plus the Reconciler that talks to
// compute-agent over NATS. See docs/architecture.md.
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

	"gitlab.com/ki.yuta1230/kyuusha/internal/authn"
	"gitlab.com/ki.yuta1230/kyuusha/internal/compute"
	"gitlab.com/ki.yuta1230/kyuusha/internal/compute/grpcserver"
	"gitlab.com/ki.yuta1230/kyuusha/internal/mtls"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/image/v1"
	networkv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	grpcAddr := flag.String("grpc-addr", ":8081", "address to serve VirtualMachineService/HypervisorService on")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	imageAddr := flag.String("image-addr", "localhost:8083", "image service address, for Create-time Image validation")
	networkAddr := flag.String("network-addr", "localhost:8084", "network service address, for Create-time NetworkInterface validation/creation")
	metricsAddr := flag.String("metrics-addr", ":9092", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers and used when dialing other services (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA both callers' and dialed services' certificates must chain to")
	bootstrapTokenPublicKey := flag.String("bootstrap-token-public-key", "hack/devkeys/jwt-dev.pub", "PEM public key verifying Hypervisor self-registration bootstrap tokens (see internal/bootstraptoken, 'kyuusha hypervisor bootstrap-token create')")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "compute", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("compute")
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

	svc, err := compute.NewService(ctx,
		identityv1.NewTenantServiceClient(identityConn),
		imagev1.NewImageServiceClient(imageConn),
		networkv1.NewSubnetServiceClient(networkConn),
		networkv1.NewNetworkInterfaceServiceClient(networkConn),
	)
	if err != nil {
		slog.Error("new compute service", "err", err)
		os.Exit(1)
	}
	recon := compute.NewReconciler(svc, nc, js)
	go func() {
		if err := recon.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("reconciler stopped", "err", err)
		}
	}()

	serverCreds, err := mtls.ServerCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		slog.Error("load mTLS server credentials", "err", err)
		os.Exit(1)
	}

	bootstrapPubKey, err := authn.LoadECDSAPublicKeyPEM(*bootstrapTokenPublicKey)
	if err != nil {
		slog.Error("load bootstrap token public key", "err", err)
		os.Exit(1)
	}

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		slog.Error("listen", "addr", *grpcAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(grpc.Creds(serverCreds), grpc.StatsHandler(otelgrpc.NewServerHandler()))
	computev1.RegisterVirtualMachineServiceServer(grpcServer, grpcserver.New(svc, recon))
	computev1.RegisterHypervisorServiceServer(grpcServer, grpcserver.NewHypervisorServer(svc, bootstrapPubKey))

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("compute: serving VirtualMachineService/HypervisorService", "addr", *grpcAddr, "network-addr", *networkAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
