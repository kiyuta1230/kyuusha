// Command compute runs the compute control-plane's gRPC API:
// VirtualMachineService and HypervisorService. Genuinely stateless -- every
// handler here is a direct etcd read/write via compute.Service, with two
// exceptions that instead relay a live NATS request/reply from within this
// same process (StreamConsole, internal/compute/console.go; and live
// Resize/AttachVolume/DetachVolume, internal/compute/liveops.go) -- neither
// needs shared state across requests, so both are just as safe at any
// replica count.
//
// All of compute's actual reconciliation (scheduling, NATS commands to
// compute-agent, retry sweeps, health sweeps) now lives in the separate
// cmd/compute-reconciler binary instead -- see its own package doc comment
// for why this was split out and the single-replica deployment invariant
// that binary depends on (docs/architecture.md
// "コントロールプレーンサービス自体の可用性" "Reconcile面": chosen instead of
// etcd-based leader election). This binary constructs a compute.Reconciler
// value purely to reuse its StreamConsole/live-hotplug methods -- it never
// calls Run(), so it does no scheduling work itself.
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
	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/compute"
	"github.com/kiyuta1230/kyuusha/internal/compute/grpcserver"
	"github.com/kiyuta1230/kyuusha/internal/etcdconn"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	grpcAddr := flag.String("grpc-addr", ":8081", "address to serve VirtualMachineService/HypervisorService on")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	imageAddr := flag.String("image-addr", "localhost:8083", "image service address, for Create-time Image validation")
	networkAddr := flag.String("network-addr", "localhost:8084", "network service address, for Create-time NetworkInterface validation/creation")
	blockStorageAddr := flag.String("block-storage-addr", "localhost:8085", "block-storage service address, for Create-time Volume validation/attachment")
	metricsAddr := flag.String("metrics-addr", ":9092", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented to callers and used when dialing other services (see internal/mtls)")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA both callers' and dialed services' certificates must chain to")
	bootstrapTokenPublicKey := flag.String("bootstrap-token-public-key", "hack/devkeys/jwt-dev.pub", "PEM public key verifying Hypervisor self-registration bootstrap tokens (see internal/bootstraptoken, 'kyuusha hypervisor bootstrap-token create')")
	etcdEndpoints := flag.String("etcd-endpoints", "etcd:2379", "comma-separated etcd endpoints (backing store, see docs/architecture.md)")
	admissionWebhookURLs := flag.String("admission-webhook-urls", "", "comma-separated external validation webhook URLs consulted synchronously on every VirtualMachine Create (see internal/admissionwebhook and docs/specs/external-integration.md \"ゲート系(作成側)\"). All must allow; empty (the default) disables this entirely")
	admissionWebhookTimeout := flag.Duration("admission-webhook-timeout", admissionwebhook.DefaultTimeout, "per-webhook timeout for -admission-webhook-urls")
	admissionWebhookFailOpen := flag.Bool("admission-webhook-fail-open", false, "if true, an unreachable/erroring webhook is treated as an implicit allow instead of denying the Create (an explicit deny from a different webhook is never overridden either way)")
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

	etcdClient, err := etcdconn.Connect(*etcdEndpoints)
	if err != nil {
		slog.Error("connect to etcd", "endpoints", *etcdEndpoints, "err", err)
		os.Exit(1)
	}
	defer etcdClient.Close()

	// Used for StreamConsole's direct NATS request/reply relay (see
	// internal/compute/console.go) and for the live-hotplug path's
	// COMPUTE_CMD publish + per-request reply-subject round trip (see
	// internal/compute/liveops.go) -- both run synchronously inside this
	// gRPC-serving process itself, without needing cmd/compute-reconciler
	// (the single-replica scheduling/NATS-command binary) in the loop.
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
	// Idempotent; defensive in case cmd/compute-reconciler hasn't created
	// COMPUTE_CMD/COMPUTE_EVT yet (either binary may start first under
	// docker-compose).
	if err := compute.EnsureStreams(ctx, js); err != nil {
		slog.Error("ensure nats streams", "err", err)
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

	blockStorageConn, err := grpc.NewClient(*blockStorageAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial block-storage", "addr", *blockStorageAddr, "err", err)
		os.Exit(1)
	}
	defer blockStorageConn.Close()

	svc, err := compute.NewService(ctx, etcdClient,
		identityv1.NewTenantServiceClient(identityConn),
		imagev1.NewImageServiceClient(imageConn),
		networkv1.NewSubnetServiceClient(networkConn),
		networkv1.NewNetworkInterfaceServiceClient(networkConn),
		blockstoragev1.NewVolumeServiceClient(blockStorageConn),
		blockstoragev1.NewVolumeAttachmentServiceClient(blockStorageConn),
	)
	if err != nil {
		slog.Error("new compute service", "err", err)
		os.Exit(1)
	}
	if *admissionWebhookURLs != "" {
		svc.AdmissionGate = admissionwebhook.Gate{
			URLs:     strings.Split(*admissionWebhookURLs, ","),
			Timeout:  *admissionWebhookTimeout,
			FailOpen: *admissionWebhookFailOpen,
		}
	}
	// recon.Run is deliberately never called here -- see this package's doc
	// comment. This value exists so grpcserver.New below can call its
	// StreamConsole and live-hotplug (LiveResize/LiveAttachVolume/
	// LiveDetachVolume) methods, both of which only ever do a direct NATS
	// round trip from within this process -- never anything that requires
	// the single-replica reconcile loop itself.
	recon := compute.NewReconciler(svc, nc, js)

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
