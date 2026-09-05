// Command compute-agent runs the NATS side of compute-agent: it accepts
// vm.create/vm.delete commands and boots real Firecracker microVMs for
// driver_hint=FIRECRACKER VMs (see internal/compute-agent/fcvmm and
// docs/specs/firecracker-boot.md); QEMU remains a stub.
package main

import (
	"context"
	"flag"
	"log/slog"
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
	"google.golang.org/grpc/credentials/insecure"

	computeagent "gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent"
	"gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent/fcvmm"
	"gitlab.com/ki.yuta1230/kyuusha/internal/telemetry"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address, for Hypervisor self-registration")
	hypervisor := flag.String("hypervisor", "", "this hypervisor's ID (required)")
	zone := flag.String("zone", "", "availability zone this hypervisor belongs to")
	vcpu := flag.Int("vcpu", 8, "allocatable vCPU capacity to report")
	memoryMB := flag.Int64("memory-mb", 16384, "allocatable memory capacity to report, in MB")
	drivers := flag.String("drivers", "FIRECRACKER", "comma-separated VMM drivers this hypervisor supports (FIRECRACKER|QEMU)")
	heartbeat := flag.Duration("heartbeat", 5*time.Second, "heartbeat interval")
	metricsAddr := flag.String("metrics-addr", ":9094", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	fcBin := flag.String("firecracker-bin", "firecracker", "firecracker binary to exec for driver_hint=FIRECRACKER VMs")
	fcCacheDir := flag.String("fc-cache-dir", "/var/lib/kyuusha/fc-cache", "directory caching downloaded kernel/rootfs artifacts, shared across VMs")
	fcRunDir := flag.String("fc-run-dir", "/var/lib/kyuusha/fc-run", "directory holding each running VM's writable rootfs copy, API socket, and console log")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if *hypervisor == "" {
		slog.Error("-hypervisor is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "compute-agent", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("compute-agent")
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

	// compute-agent -> compute is plaintext for now; see api-gateway's
	// identical note on the mTLS follow-up. Also unauthenticated: the real
	// design verifies a zone-scoped bootstrap token here (see
	// docs/architecture.md and docs/open-questions.md).
	computeConn, err := grpc.NewClient(*computeAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()

	agent := &computeagent.Agent{
		Hypervisor:          *hypervisor,
		NC:                  nc,
		JS:                  js,
		HeartbeatInterval:   *heartbeat,
		Hypervisors:         computev1.NewHypervisorServiceClient(computeConn),
		Zone:                *zone,
		AllocatableVCPU:     int32(*vcpu),
		AllocatableMemoryMB: *memoryMB,
		SupportedDrivers:    strings.Split(*drivers, ","),
		Firecracker: &fcvmm.Manager{
			BinPath:  *fcBin,
			CacheDir: *fcCacheDir,
			RunDir:   *fcRunDir,
		},
	}
	slog.Info("compute-agent: starting", "hypervisor", *hypervisor, "zone", *zone)
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}
