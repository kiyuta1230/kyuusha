// Command network-reconciler runs only network's reconcile loop
// (network.Service.Run): vlan_id/ip_address/mac_address allocation for
// every Subnet/NetworkInterface CreateSubnet/CreateNetworkInterface leave
// Pending, and the orphaned-NetworkInterface sweep. It serves no gRPC API
// at all -- cmd/network is the SubnetService/NetworkInterfaceService gRPC
// binary, safely run as any number of stateless replicas (see its own
// package doc comment).
//
// Deployment invariant this binary depends on: run exactly one replica of
// network-reconciler at a time -- see cmd/compute-reconciler's identical
// invariant and doc comment for the full reasoning (docs/architecture.md
// "コントロールプレーンサービス自体の可用性" "Reconcile面"). Two replicas
// running simultaneously could both allocate from the same
// vlanPool/ipPool/nextMACOct independently, since none of them are
// actually shared state -- each replica's copy would drift the moment
// either one allocates, handing out the same vlan_id/ip_address/
// mac_address to two different Subnets/NetworkInterfaces.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address, for the orphaned-NetworkInterface sweep (does this NetworkInterface's vm_id still exist?)")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	metricsAddr := flag.String("metrics-addr", ":9099", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate used when dialing compute (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA compute's certificate must chain to")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	vlanRangesFlag := flag.String("vlan-ranges", "", `VLAN IDs each zone may hand out to new Subnets, "<zone>=<lo>-<hi>[,<lo>-<hi>...][;<zone>=...]" ("*" = every zone not listed); empty = 1-4094 everywhere (see docs/specs/network.md)`)
	flag.Parse()
	vlanRanges, err := network.ParseVLANRanges(*vlanRangesFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "network-reconciler", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("network-reconciler")
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

	identityConn, err := grpc.NewClient(*identityAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial identity", "addr", *identityAddr, "err", err)
		os.Exit(1)
	}
	defer identityConn.Close()

	// A second, independent network.Service instance from the API binary's
	// own -- both need one (Run's methods read/write through it), but
	// neither shares process memory (including vlanPool/ipPool/
	// nextMACOct) with the other, only the etcd state both connect to.
	// js is nil: this binary serves no gRPC API (no UpdateFirewallRules
	// handler ever runs here), so publishUpdateACL's NATS notify path is
	// simply never reached -- see cmd/network/main.go for the binary that
	// does need it.
	svc, err := network.NewService(ctx, etcdClient, identityv1.NewTenantServiceClient(identityConn), computev1.NewVirtualMachineServiceClient(computeConn), nil)
	if err != nil {
		slog.Error("new network service", "err", err)
		os.Exit(1)
	}
	svc.SetVLANRanges(vlanRanges)
	prometheus.MustRegister(network.NewMetricsCollector(svc))

	slog.Info("network-reconciler: starting", "vlan_ranges", *vlanRangesFlag)
	if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("reconciler stopped", "err", err)
		os.Exit(1)
	}
}
