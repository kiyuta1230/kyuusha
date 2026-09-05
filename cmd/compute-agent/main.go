// Command compute-agent runs the NATS side of compute-agent: it accepts
// vm.create commands and reports success, but does not talk to a real VMM
// yet. See docs/architecture.md and internal/compute-agent.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	computeagent "gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent"

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
	flag.Parse()

	if *hypervisor == "" {
		slog.Error("-hypervisor is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	computeConn, err := grpc.NewClient(*computeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
	}
	slog.Info("compute-agent: starting", "hypervisor", *hypervisor, "zone", *zone)
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}
