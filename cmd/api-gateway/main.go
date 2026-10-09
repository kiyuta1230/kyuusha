// Command api-gateway is the single place that terminates client JWTs and
// runs OPA authorization (docs/architecture.md "認証・認可とHypervisor登録").
// Backend services like compute are meant to be reached only through it.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/authz"
	"github.com/kiyuta1230/kyuusha/internal/gateway"
	"github.com/kiyuta1230/kyuusha/internal/mtls"
	"github.com/kiyuta1230/kyuusha/internal/telemetry"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func main() {
	listenAddr := flag.String("listen-addr", ":8080", "address to serve the client-facing API on")
	computeAddr := flag.String("compute-addr", "localhost:8081", "compute service address")
	identityAddr := flag.String("identity-addr", "localhost:8082", "identity service address")
	imageAddr := flag.String("image-addr", "localhost:8083", "image service address")
	networkAddr := flag.String("network-addr", "localhost:8084", "network service address")
	blockStorageAddr := flag.String("block-storage-addr", "localhost:8085", "block-storage service address")
	jwtPublicKey := flag.String("jwt-public-key", "hack/devkeys/jwt-dev.pub", "PEM public key file to verify client JWTs against (dev/test; ignored if -jwt-jwks-url is set)")
	jwtJWKSURL := flag.String("jwt-jwks-url", "", "JWKS endpoint to verify client JWTs against (e.g. a Keycloak realm's .../protocol/openid-connect/certs); takes precedence over -jwt-public-key")
	jwtIssuer := flag.String("jwt-issuer", "", "required \"iss\" of client JWTs (e.g. a Keycloak realm's https://<host>/realms/<realm>); empty accepts any. Set it, with -jwt-audience, whenever the issuer also issues tokens for other applications -- see docs/specs/authn-authz.md")
	jwtAudience := flag.String("jwt-audience", "", "value client JWTs' \"aud\" must contain (e.g. \"kyuusha\"); empty accepts any")
	metricsAddr := flag.String("metrics-addr", ":9093", "address to serve /metrics (Prometheus) on")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector address (empty disables tracing)")
	tlsCert := flag.String("tls-cert", "hack/devcerts/server.crt", "east-west mTLS certificate presented when dialing backend services (see internal/mtls); unrelated to the client-facing JWT above")
	tlsKey := flag.String("tls-key", "hack/devcerts/server.key", "east-west mTLS private key")
	tlsCA := flag.String("tls-ca", "hack/devcerts/ca.crt", "CA backend services' certificates must chain to")
	externalBackendsFlag := flag.String("external-backends", "", `extra gRPC backends this gateway fronts without knowing their proto, "<service prefix>=<address>[,...]" (e.g. "kyuusha.vpc.v1.=vpc-api:9000"); same authn/authz/audit/mTLS as built-in services -- see docs/specs/external-integration.md`)
	externalDescriptorsFlag := flag.String("external-backend-descriptor-sets", "", `optional "<service prefix>=<FileDescriptorSet file>[,...]" for -external-backends routes, used instead of asking the backend via gRPC server reflection`)
	flag.Parse()

	// JSON structured logging (docs/architecture.md's Observability design),
	// so a log pipeline (Loki in the playground) can parse fields like the
	// audit records internal/audit emits.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "api-gateway", *otlpEndpoint)
	if err != nil {
		slog.Error("setup tracing", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics("api-gateway")
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

	var verifier *authn.Verifier
	if *jwtJWKSURL != "" {
		verifier, err = authn.NewJWKSVerifier(ctx, *jwtJWKSURL)
		if err != nil {
			slog.Error("setup jwks verifier", "url", *jwtJWKSURL, "err", err)
			os.Exit(1)
		}
	} else {
		pubKey, err := authn.LoadECDSAPublicKeyPEM(*jwtPublicKey)
		if err != nil {
			slog.Error("load jwt public key", "err", err)
			os.Exit(1)
		}
		verifier = authn.NewStaticKeyVerifier(pubKey)
	}
	verifier.Issuer, verifier.Audience = *jwtIssuer, *jwtAudience
	if *jwtIssuer == "" || *jwtAudience == "" {
		slog.Warn("api-gateway: client JWTs' issuer and/or audience are not checked (-jwt-issuer/-jwt-audience); any token signed by the configured key is accepted")
	}

	authorizer, err := authz.New(ctx)
	if err != nil {
		slog.Error("prepare authorizer", "err", err)
		os.Exit(1)
	}

	clientCreds, err := mtls.ClientCredentials(*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		slog.Error("load mTLS client credentials", "err", err)
		os.Exit(1)
	}

	computeConn, err := grpc.NewClient(*computeAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(authn.PropagateCallerUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(authn.PropagateCallerStreamInterceptor()),
	)
	if err != nil {
		slog.Error("dial compute", "addr", *computeAddr, "err", err)
		os.Exit(1)
	}
	defer computeConn.Close()
	vmProxy := gateway.NewVirtualMachineProxy(computev1.NewVirtualMachineServiceClient(computeConn))
	hypervisorProxy := gateway.NewHypervisorProxy(computev1.NewHypervisorServiceClient(computeConn))

	identityConn, err := grpc.NewClient(*identityAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(authn.PropagateCallerUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(authn.PropagateCallerStreamInterceptor()),
	)
	if err != nil {
		slog.Error("dial identity", "addr", *identityAddr, "err", err)
		os.Exit(1)
	}
	defer identityConn.Close()
	tenantProxy := gateway.NewTenantProxy(identityv1.NewTenantServiceClient(identityConn))

	imageConn, err := grpc.NewClient(*imageAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(authn.PropagateCallerUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(authn.PropagateCallerStreamInterceptor()),
	)
	if err != nil {
		slog.Error("dial image", "addr", *imageAddr, "err", err)
		os.Exit(1)
	}
	defer imageConn.Close()
	imageProxy := gateway.NewImageProxy(imagev1.NewImageServiceClient(imageConn))

	networkConn, err := grpc.NewClient(*networkAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(authn.PropagateCallerUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(authn.PropagateCallerStreamInterceptor()),
	)
	if err != nil {
		slog.Error("dial network", "addr", *networkAddr, "err", err)
		os.Exit(1)
	}
	defer networkConn.Close()
	subnetProxy := gateway.NewSubnetProxy(networkv1.NewSubnetServiceClient(networkConn))
	networkInterfaceProxy := gateway.NewNetworkInterfaceProxy(networkv1.NewNetworkInterfaceServiceClient(networkConn))

	blockStorageConn, err := grpc.NewClient(*blockStorageAddr,
		grpc.WithTransportCredentials(clientCreds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(authn.PropagateCallerUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(authn.PropagateCallerStreamInterceptor()),
	)
	if err != nil {
		slog.Error("dial block-storage", "addr", *blockStorageAddr, "err", err)
		os.Exit(1)
	}
	defer blockStorageConn.Close()
	volumeProxy := gateway.NewVolumeProxy(blockstoragev1.NewVolumeServiceClient(blockStorageConn))
	volumeAttachmentProxy := gateway.NewVolumeAttachmentProxy(blockstoragev1.NewVolumeAttachmentServiceClient(blockStorageConn))
	storageConnectionProxy := gateway.NewStorageConnectionProxy(blockstoragev1.NewStorageConnectionServiceClient(blockStorageConn))

	externalRoutes, err := gateway.ParseExternalBackends(*externalBackendsFlag)
	if err != nil {
		slog.Error("parse -external-backends", "err", err)
		os.Exit(1)
	}
	descriptorFiles, err := gateway.ParseExternalBackends(*externalDescriptorsFlag) // same "<prefix>=<value>" shape
	if err != nil {
		slog.Error("parse -external-backend-descriptor-sets", "err", err)
		os.Exit(1)
	}
	var external gateway.ExternalBackends
	// The rest of network's own package (AllocationPool/NetworkClass/
	// Network services) rides the same generic forwarding as an external
	// backend, with descriptors from this binary's own compiled-in protos
	// -- no hand-written proxy per service. Built-in proxies above still
	// win for the services they register.
	external.AddRoute("kyuusha.network.v1.", networkConn, protoregistry.GlobalFiles)
	// Same for compute's HostAggregateService (VM/Hypervisor keep their
	// built-in proxies).
	external.AddRoute("kyuusha.compute.v1.", computeConn, protoregistry.GlobalFiles)
	for prefix, addr := range externalRoutes {
		conn, err := grpc.NewClient(addr,
			grpc.WithTransportCredentials(clientCreds),
			grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
			grpc.WithChainUnaryInterceptor(authn.PropagateCallerUnaryInterceptor()),
			grpc.WithChainStreamInterceptor(authn.PropagateCallerStreamInterceptor()),
		)
		if err != nil {
			slog.Error("dial external backend", "prefix", prefix, "addr", addr, "err", err)
			os.Exit(1)
		}
		defer conn.Close()
		var files *protoregistry.Files
		if path, ok := descriptorFiles[prefix]; ok {
			raw, err := os.ReadFile(path)
			if err == nil {
				files, err = gateway.FilesFromDescriptorSet(raw)
			}
			if err != nil {
				slog.Error("load external backend descriptor set", "prefix", prefix, "path", path, "err", err)
				os.Exit(1)
			}
		}
		external.AddRoute(prefix, conn, files)
		slog.Info("api-gateway: external backend registered", "prefix", prefix, "addr", addr)
	}

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		slog.Error("listen", "addr", *listenAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(verifier.UnaryInterceptor(), authorizer.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(verifier.StreamInterceptor(), authorizer.StreamInterceptor()),
		// Unregistered services go to external backends (or Unimplemented),
		// behind the same stream interceptors; the codec lets them see raw
		// frames while every built-in service still gets plain proto.
		grpc.UnknownServiceHandler(external.Handler()),
		grpc.ForceServerCodecV2(gateway.ServerCodec()),
	)
	computev1.RegisterVirtualMachineServiceServer(grpcServer, vmProxy)
	computev1.RegisterHypervisorServiceServer(grpcServer, hypervisorProxy)
	identityv1.RegisterTenantServiceServer(grpcServer, tenantProxy)
	imagev1.RegisterImageServiceServer(grpcServer, imageProxy)
	networkv1.RegisterSubnetServiceServer(grpcServer, subnetProxy)
	networkv1.RegisterNetworkInterfaceServiceServer(grpcServer, networkInterfaceProxy)
	blockstoragev1.RegisterVolumeServiceServer(grpcServer, volumeProxy)
	blockstoragev1.RegisterVolumeAttachmentServiceServer(grpcServer, volumeAttachmentProxy)
	blockstoragev1.RegisterStorageConnectionServiceServer(grpcServer, storageConnectionProxy)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	slog.Info("api-gateway: serving", "addr", *listenAddr, "compute-addr", *computeAddr, "identity-addr", *identityAddr, "image-addr", *imageAddr, "network-addr", *networkAddr, "block-storage-addr", *blockStorageAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("grpc serve", "err", err)
		os.Exit(1)
	}
}
