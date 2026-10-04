package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
	"github.com/kiyuta1230/kyuusha/internal/authn"
	"github.com/kiyuta1230/kyuusha/internal/authz"
)

// fakeSubnetBackend stands in for an external backend the gateway knows
// nothing about at compile time: kyuusha's own SubnetService proto is just
// a convenient real service to serve (the test gateway never registers it
// as a built-in, so every call takes the external-backend path).
type fakeSubnetBackend struct {
	networkv1.UnimplementedSubnetServiceServer
	mu      sync.Mutex
	calls   int
	callers []string // x-kyuusha-caller-sub seen per call
}

func (b *fakeSubnetBackend) record(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	sub, _ := authn.CallerSubFromContext(ctx)
	b.callers = append(b.callers, sub)
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("authorization")) > 0 {
		b.callers = append(b.callers, "LEAKED-AUTHORIZATION")
	}
}

func (b *fakeSubnetBackend) Get(ctx context.Context, req *networkv1.GetSubnetRequest) (*networkv1.Subnet, error) {
	b.record(ctx)
	if req.GetId() == "missing" {
		return nil, status.Error(codes.NotFound, "subnet: not found")
	}
	return &networkv1.Subnet{Meta: &resourcev1.ObjectMeta{Id: req.GetId(), TenantId: req.GetTenantId()}}, nil
}

func (b *fakeSubnetBackend) Watch(req *networkv1.WatchSubnetsRequest, stream networkv1.SubnetService_WatchServer) error {
	b.record(stream.Context())
	for _, id := range []string{"subnet-1", "subnet-2"} {
		if err := stream.Send(&networkv1.SubnetEvent{Type: networkv1.SubnetEvent_ADDED, Subnet: &networkv1.Subnet{Meta: &resourcev1.ObjectMeta{Id: id}}}); err != nil {
			return err
		}
	}
	return nil
}

func serve(t *testing.T, s *grpc.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

func TestExternalBackendProxy(t *testing.T) {
	ctx := context.Background()

	backend := &fakeSubnetBackend{}
	bs := grpc.NewServer()
	networkv1.RegisterSubnetServiceServer(bs, backend)
	reflection.Register(bs)
	backendAddr := serve(t, bs)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := authz.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	verifier := authn.NewStaticKeyVerifier(&key.PublicKey)

	backendConn, err := grpc.NewClient(backendAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(authn.PropagateCallerUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(authn.PropagateCallerStreamInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backendConn.Close()
	var ext ExternalBackends
	ext.AddRoute("kyuusha.network.v1.", backendConn, nil)
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(verifier.UnaryInterceptor(), authorizer.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(verifier.StreamInterceptor(), authorizer.StreamInterceptor()),
		grpc.UnknownServiceHandler(ext.Handler()),
		grpc.ForceServerCodecV2(ServerCodec()),
	)
	gwAddr := serve(t, gs)
	gwConn, err := grpc.NewClient(gwAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer gwConn.Close()

	tokenWithRole := func(tenant, sub, role string) context.Context {
		signed, err := jwt.NewWithClaims(jwt.SigningMethodES256, &authn.Claims{
			TenantID:         tenant,
			Role:             role,
			RegisteredClaims: jwt.RegisteredClaims{Subject: sub, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
		}).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+signed)
	}
	token := func(tenant, sub string) context.Context { return tokenWithRole(tenant, sub, "") }
	alice := token("tenant-a", "alice")
	subnets := networkv1.NewSubnetServiceClient(gwConn)

	t.Run("own tenant's unary call is forwarded with the caller propagated", func(t *testing.T) {
		got, err := subnets.Get(alice, &networkv1.GetSubnetRequest{TenantId: "tenant-a", Id: "subnet-1"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.GetMeta().GetId() != "subnet-1" {
			t.Fatalf("Get returned %v", got)
		}
		backend.mu.Lock()
		defer backend.mu.Unlock()
		if last := backend.callers[len(backend.callers)-1]; last != "alice" {
			t.Fatalf("backend saw caller %q (callers %v), want alice and no client authorization header", last, backend.callers)
		}
	})

	t.Run("another tenant is denied before reaching the backend", func(t *testing.T) {
		backend.mu.Lock()
		before := backend.calls
		backend.mu.Unlock()
		_, err := subnets.Get(alice, &networkv1.GetSubnetRequest{TenantId: "tenant-b", Id: "subnet-1"})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Get for tenant-b: got %v, want PermissionDenied", err)
		}
		backend.mu.Lock()
		defer backend.mu.Unlock()
		if backend.calls != before {
			t.Fatal("a denied call still reached the backend")
		}
	})

	t.Run("backend errors pass through", func(t *testing.T) {
		_, err := subnets.Get(alice, &networkv1.GetSubnetRequest{TenantId: "tenant-a", Id: "missing"})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("Get missing: got %v, want NotFound", err)
		}
	})

	t.Run("server streaming is forwarded and authorized", func(t *testing.T) {
		stream, err := subnets.Watch(alice, &networkv1.WatchSubnetsRequest{TenantId: "tenant-a"})
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		var ids []string
		for {
			ev, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Watch Recv: %v", err)
			}
			ids = append(ids, ev.GetSubnet().GetMeta().GetId())
		}
		if len(ids) != 2 {
			t.Fatalf("Watch delivered %v, want 2 events", ids)
		}
		denied, err := subnets.Watch(alice, &networkv1.WatchSubnetsRequest{TenantId: "tenant-b"})
		if err == nil {
			_, err = denied.Recv()
		}
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Watch for tenant-b: got %v, want PermissionDenied", err)
		}
	})

	t.Run("a service with no registered backend is Unimplemented", func(t *testing.T) {
		_, err := identityv1.NewTenantServiceClient(gwConn).Get(tokenWithRole("ops", "root", "admin"), &identityv1.GetTenantRequest{})
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("unrouted service: got %v, want Unimplemented", err)
		}
	})
}

func TestParseExternalBackends(t *testing.T) {
	got, err := ParseExternalBackends("kyuusha.vpc.v1.=vpc:9000, acme.=acme:1")
	if err != nil || got["kyuusha.vpc.v1."] != "vpc:9000" || got["acme."] != "acme:1" {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, bad := range []string{"novalue", "=x:1", "kyuusha.vpc=x:1"} {
		if _, err := ParseExternalBackends(bad); err == nil {
			t.Errorf("ParseExternalBackends(%q) succeeded, want an error", bad)
		}
	}
}
