// Command network runs the network control-plane's gRPC API:
// SubnetService and NetworkInterfaceService. Every handler is a direct
// etcd read/write via network.Service -- Create leaves every Subnet/
// NetworkInterface Pending, never attempting vlan_id/ip_address/
// mac_address allocation itself (see network.Service.CreateSubnet's doc
// comment), so this binary touches none of vlanPool/ipPool/nextMACOct. The
// orphan-GC sweep is this Service's only *other* user of a compute client,
// and only runs from Run, which this binary never calls.
//
// This binary does, however, dial compute and NATS/JetStream -- purely so
// UpdateFirewallRules can resolve which hypervisor is running a
// NetworkInterface's VM and notify it (see internal/network/nats.go's
// publishUpdateACL). This is the same exception cmd/compute/main.go already
// documents for itself (StreamConsole/live-hotplug: a synchronous
// request/notify from within the stateless API process, not the
// reconciler's exclusive-single-replica scheduling/allocation state) --
// every replica dials compute/NATS independently and does a pure
// request/response plus fire-and-forget publish, so no shared in-memory
// state crosses replicas and this binary remains safe to run at any
// replica count.
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

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/network/grpcserver"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8084", "address to serve SubnetService/NetworkInterfaceService on")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address, for UpdateFirewallRules to resolve which hypervisor is running a NetworkInterface's VM")
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL, for UpdateFirewallRules to notify the owning hypervisor")
	metricsAddr := flag.String("metrics-addr", ":9096", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA callers' certificates must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	admissionWebhookURLs := flag.String("admission-webhook-urls", "", "comma-separated external validation webhook URLs consulted synchronously on Subnet Create/Update/Delete and NetworkInterface Create/Update/UpdateFirewallRules (see docs/specs/external-integration.md \"ゲート系(作成側)\"). All must allow; empty (the default) disables this entirely")
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

	computeConn, err := grpc.NewClient(*computeAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()

	etcdClient, err := etcdconn.Connect(*etcdEndpoints)
	if err != nil {
		slog.Error("connect to etcd", "endpoints", *etcdEndpoints, "err", err)
		os.Exit(1)
	}
	defer etcdClient.Close()

	// See this package's doc comment: this is UpdateFirewallRules'
	// notify-the-owning-hypervisor path only, not the reconciler's
	// allocation state.
	nc, err := nats.Connect(*natsURL)
	if err != nil {
		slog.Error("connect to nats", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		slog.Error("jetstream", "err", err)
		os.Exit(1)
	}
	// Idempotent; defensive in case cmd/network-reconciler or a
	// compute-agent hasn't created NETWORK_CMD yet (any of them may start
	// first under docker-compose).
	if err := network.EnsureStreams(ctx, js); err != nil {
		slog.Error("ensure nats streams", "err", err)
		os.Exit(1)
	}

	// Reconcile (vlan_id/ip_address/mac_address allocation, orphan sweep)
	// deliberately never runs here -- see this package's doc comment.
	svc, err := network.NewService(ctx, etcdClient, identityv1.NewTenantServiceClient(identityConn), computev1.NewVirtualMachineServiceClient(computeConn), js)
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
