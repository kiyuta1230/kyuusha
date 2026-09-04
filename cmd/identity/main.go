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
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"gitlab.com/ki.yuta1230/kyuusha/internal/identity"
	"gitlab.com/ki.yuta1230/kyuusha/internal/identity/grpcserver"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func main() {
	grpcAddr := flag.String("grpc-addr", ":8082", "address to serve TenantService on")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc := identity.NewService()

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		slog.Error("listen", "addr", *grpcAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer()
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
