// Command api-gateway is the single place that terminates client JWTs and
// runs OPA authorization (docs/architecture.md "認証・認可とHypervisor登録").
// Backend services like compute are meant to be reached only through it.
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
	"google.golang.org/grpc/credentials/insecure"

	"gitlab.com/ki.yuta1230/kyuusha/internal/authn"
	"gitlab.com/ki.yuta1230/kyuusha/internal/authz"
	"gitlab.com/ki.yuta1230/kyuusha/internal/gateway"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

func main() {
	listenAddr := flag.String("listen-addr", ":8080", "address to serve the client-facing API on")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address")
	jwtPublicKey := flag.String("jwt-public-key", "hack/devkeys/jwt-dev.pub", "PEM file used to verify client JWTs")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pubKey, err := authn.LoadECDSAPublicKeyPEM(*jwtPublicKey)
	if err != nil {
		slog.Error("load jwt public key", "err", err)
		os.Exit(1)
	}
	verifier := &authn.Verifier{PublicKey: pubKey}

	authorizer, err := authz.New(ctx)
	if err != nil {
		slog.Error("prepare authorizer", "err", err)
		os.Exit(1)
	}

	// api-gateway -> compute is plaintext for now; see docs/architecture.md's
	// mTLS design for the follow-up (backends should only trust api-gateway,
	// not accept unauthenticated connections from anywhere on the network).
	computeConn, err := grpc.NewClient(*computeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()
	vmProxy := gateway.NewVirtualMachineProxy(computev1.NewVirtualMachineServiceClient(computeConn))

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		slog.Error("listen", "addr", *listenAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(verifier.UnaryInterceptor(), authorizer.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(verifier.StreamInterceptor(), authorizer.StreamInterceptor()),
	)
	computev1.RegisterVirtualMachineServiceServer(grpcServer, vmProxy)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("api-gateway: serving", "addr", *listenAddr, "compute-addr", *computeAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
