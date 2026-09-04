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
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	computeagent "gitlab.com/ki.yuta1230/kyuusha/internal/compute-agent"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	hypervisor := flag.String("hypervisor", "", "this hypervisor's ID (required, must match -hypervisors on compute)")
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

	agent := &computeagent.Agent{Hypervisor: *hypervisor, NC: nc, JS: js, HeartbeatInterval: *heartbeat}
	slog.Info("compute-agent: starting", "hypervisor", *hypervisor)
	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}
