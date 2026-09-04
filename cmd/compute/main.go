// Command compute runs the compute control-plane: the VirtualMachineService
// gRPC API plus the Reconciler that talks to compute-agent over NATS. See
// docs/architecture.md. The scheduler is still a round-robin stub over
// -hypervisors; there is no real Hypervisor inventory yet.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"gitlab.com/ki.yuta1230/kyuusha/internal/compute"
	"gitlab.com/ki.yuta1230/kyuusha/internal/compute/grpcserver"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	natsURL := flag.String("nats-url", nats.DefaultURL, "NATS server URL")
	grpcAddr := flag.String("grpc-addr", ":8081", "address to serve VirtualMachineService on")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address, for Create-time Quota checks")
	hypervisors := flag.String("hypervisors", "hypervisor-1", "comma-separated list of hypervisor IDs the stub scheduler may pick from")
	flag.Parse()

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

	// compute -> identity is plaintext for now; see api-gateway's identical
	// note on the mTLS follow-up.
	identityConn, err := grpc.NewClient(*identityAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("dial identity", "addr", *identityAddr, "err", err)
		os.Exit(1)
	}
	defer identityConn.Close()

	svc, err := compute.NewService(ctx, identityv1.NewTenantServiceClient(identityConn))
	if err != nil {
		slog.Error("new compute service", "err", err)
		os.Exit(1)
	}
	recon := compute.NewReconciler(svc, nc, js, strings.Split(*hypervisors, ","))
	go func() {
		if err := recon.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("reconciler stopped", "err", err)
		}
	}()

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		slog.Error("listen", "addr", *grpcAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer()
	computev1.RegisterVirtualMachineServiceServer(grpcServer, grpcserver.New(svc))

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("compute: serving VirtualMachineService", "addr", *grpcAddr, "hypervisors", *hypervisors)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
