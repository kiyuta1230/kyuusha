// Command identity runs the identity control-plane: the TenantService gRPC
// API. See docs/architecture.md "identityサービスのリソース: Tenant". Unlike
// compute, there's no NATS/agent side and no reconciler: creating a Tenant
// record is the entire action, so Create sets it Active synchronously.
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

	"gitlab.com/ki.yuta1230/kyuusha/internal/etcdconn"
	"gitlab.com/ki.yuta1230/kyuusha/internal/identity"
	"gitlab.com/ki.yuta1230/kyuusha/internal/identity/grpcserver"
	"gitlab.com/ki.yuta1230/kyuusha/internal/mtls"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8082", "address to serve TenantService on")
	metricsAddr := flag.String("metrics-addr", ":9091", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA callers' certificates must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "identity", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("identity")
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

	svc := identity.NewService(etcdClient)

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
	identityv1.RegisterTenantServiceServer(grpcServer, grpcserver.New(svc))

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("identity: serving TenantService", "addr", *grpcAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
