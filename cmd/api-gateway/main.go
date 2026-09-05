// Command api-gateway is the single place that terminates client JWTs and
// runs OPA authorization (docs/architecture.md "認証・認可とHypervisor登録").
// Backend services like compute are meant to be reached only through it.
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

	"gitlab.com/ki.yuta1230/kyuusha/internal/authn"
	"gitlab.com/ki.yuta1230/kyuusha/internal/authz"
	"gitlab.com/ki.yuta1230/kyuusha/internal/gateway"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

func main() {
	listenAddr := flag.String("listen-addr", ":8080", "address to serve the client-facing API on")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address")
	imageAddr := flag.String("image-addr", "localhost:8083", "image service address")
	jwtPublicKey := flag.String("jwt-public-key", "hack/devkeys/jwt-dev.pub", "PEM public key file to verify client JWTs against (dev/test; ignored if -jwt-jwks-url is set)")
	jwtJWKSURL := flag.String("jwt-jwks-url", "", "JWKS endpoint to verify client JWTs against (e.g. a Keycloak realm's .../protocol/openid-connect/certs); takes precedence over -jwt-public-key")
	metricsAddr := flag.String("metrics-addr", ":9093", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	flag.Parse()

	// JSON structured logging (docs/architecture.md's Observability design),
	// so a log pipeline (Loki in the playground) can parse fields like the
	// audit records internal/audit emits.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "api-gateway", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("api-gateway")
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

	var verifier *authn.Verifier
	if *jwtJWKSURL != "" {
		verifier, err = authn.NewJWKSVerifier(ctx, *jwtJWKSURL)
		if err != nil {
			slog.Error("setup jwks verifier", "url", *jwtJWKSURL, "err", err)
			os.Exit(1)
		}
	} else {
		pubKey, err := authn.LoadECDSAPublicKeyPEM(*jwtPublicKey)
		if err != nil {
			slog.Error("load jwt public key", "err", err)
			os.Exit(1)
		}
		verifier = authn.NewStaticKeyVerifier(pubKey)
	}

	authorizer, err := authz.New(ctx)
	if err != nil {
		slog.Error("prepare authorizer", "err", err)
		os.Exit(1)
	}

	// api-gateway -> compute is plaintext for now; see docs/architecture.md's
	// mTLS design for the follow-up (backends should only trust api-gateway,
	// not accept unauthenticated connections from anywhere on the network).
	computeConn, err := grpc.NewClient(*computeAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()
	vmProxy := gateway.NewVirtualMachineProxy(computev1.NewVirtualMachineServiceClient(computeConn))
	hypervisorProxy := gateway.NewHypervisorProxy(computev1.NewHypervisorServiceClient(computeConn))

	identityConn, err := grpc.NewClient(*identityAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial identity", "addr", *identityAddr, "err", err)
		os.Exit(1)
	}
	defer identityConn.Close()
	tenantProxy := gateway.NewTenantProxy(identityv1.NewTenantServiceClient(identityConn))

	imageConn, err := grpc.NewClient(*imageAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial image", "addr", *imageAddr, "err", err)
		os.Exit(1)
	}
	defer imageConn.Close()
	imageProxy := gateway.NewImageProxy(imagev1.NewImageServiceClient(imageConn))

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		slog.Error("listen", "addr", *listenAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(verifier.UnaryInterceptor(), authorizer.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(verifier.StreamInterceptor(), authorizer.StreamInterceptor()),
	)
	computev1.RegisterVirtualMachineServiceServer(grpcServer, vmProxy)
	computev1.RegisterHypervisorServiceServer(grpcServer, hypervisorProxy)
	identityv1.RegisterTenantServiceServer(grpcServer, tenantProxy)
	imagev1.RegisterImageServiceServer(grpcServer, imageProxy)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("api-gateway: serving", "addr", *listenAddr, "compute-addr", *computeAddr, "identity-addr", *identityAddr, "image-addr", *imageAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
