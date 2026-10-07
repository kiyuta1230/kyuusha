// Command network runs the network control-plane's gRPC API:
// SubnetService and NetworkInterfaceService. Every handler is a direct
// etcd read/write via network.Service -- Create leaves every Subnet/
// NetworkInterface Pending, never attempting vlan_id/ip_address/
// mac_address allocation itself (see network.Service.CreateSubnet's doc
// comment), so this binary touches none of vlanPool/ipPool/nextMACOct. The
// orphan-GC sweep is this Service's only *other* user of a compute client,
// and only runs from Run, which this binary never calls.
//
// It also serves PolicyDistributionService, compute-agents' xDS-like
// policy streams (see network.PolicyHub): each replica keeps its own
// read-only cache of NetworkInterfaces/SecurityGroups from an etcd watch
// and answers its streams from it, so this binary stays safe to run at any
// replica count -- an agent may connect to whichever replica.
//
// All actual allocation (plus the orphan-GC sweep) lives in the separate
// cmd/network-reconciler binary instead -- see its own package doc
// comment for the single-replica deployment invariant that binary depends
// on (2026-09-13, mirroring cmd/compute-reconciler's split from cmd/compute:
// docs/architecture.md "コントロールプレーンサービス自体の可用性" "Reconcile面").
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/network/grpcserver"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	networkagentv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/agent/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8084", "address to serve SubnetService/NetworkInterfaceService on")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	metricsAddr := flag.String("metrics-addr", ":9096", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA callers' certificates must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	admissionWebhookURLs := flag.String("admission-webhook-urls", "", "comma-separated external validation webhook URLs consulted synchronously on Subnet Create/Update/Delete and NetworkInterface Create/Update/SetSecurityGroups and SecurityGroup Create/Update/Delete (see docs/specs/external-integration.md \"ゲート系(作成側)\"). All must allow; empty (the default) disables this entirely")
	admissionWebhookTimeout := flag.Duration("admission-webhook-timeout", admissionwebhook.DefaultTimeout, "per-webhook timeout for -admission-webhook-urls")
	admissionWebhookFailOpen := flag.Bool("admission-webhook-fail-open", false, "if true, an unreachable/erroring webhook is treated as an implicit allow instead of denying the request (an explicit deny from a different webhook is never overridden either way)")
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

	etcdClient, err := etcdconn.Connect(*etcdEndpoints)
	if err != nil {
		slog.Error("connect to etcd", "endpoints", *etcdEndpoints, "err", err)
		os.Exit(1)
	}
	defer etcdClient.Close()

	// Reconcile (vlan_id/ip_address/mac_address allocation, orphan sweep)
	// deliberately never runs here -- see this package's doc comment.
	svc, err := network.NewService(ctx, etcdClient, identityv1.NewTenantServiceClient(identityConn), nil)
	if err != nil {
		slog.Error("new network service", "err", err)
		os.Exit(1)
	}
	if *admissionWebhookURLs != "" {
		svc.AdmissionGate = admissionwebhook.Gate{
			URLs:     strings.Split(*admissionWebhookURLs, ","),
			Timeout:  *admissionWebhookTimeout,
			FailOpen: *admissionWebhookFailOpen,
		}
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
	networkv1.RegisterSubnetServiceServer(grpcServer, grpcserver.NewSubnetServer(svc))
	networkv1.RegisterNetworkInterfaceServiceServer(grpcServer, grpcserver.NewNetworkInterfaceServer(svc))
	networkv1.RegisterNetworkServiceServer(grpcServer, grpcserver.NewNetworkServer(svc))
	networkv1.RegisterNetworkClassServiceServer(grpcServer, grpcserver.NewNetworkClassServer(svc))
	networkv1.RegisterAllocationPoolServiceServer(grpcServer, grpcserver.NewAllocationPoolServer(svc))
	networkv1.RegisterSecurityGroupServiceServer(grpcServer, grpcserver.NewSecurityGroupServer(svc))
	networkv1.RegisterIPReservationServiceServer(grpcServer, grpcserver.NewIPReservationServer(svc))
	// compute-agents' policy streams (east-west; see network.PolicyHub).
	// Every replica runs its own hub from etcd -- nothing here is shared
	// state, so an agent may connect to any replica.
	hub := network.NewPolicyHub(svc)
	go hub.Run(ctx)
	networkagentv1.RegisterPolicyDistributionServiceServer(grpcServer, grpcserver.NewPolicyDistributionServer(hub))

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
