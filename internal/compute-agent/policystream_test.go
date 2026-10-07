package computeagent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"

	networkagentv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/agent/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// policyVMM is a vmm.VMM stub whose Running/ApplyACL the policy stream uses.
type policyVMM struct {
	mu      sync.Mutex
	running []vmm.RunningVM
	applied []vmm.ACLUpdate
	fail    error
}

func (f *policyVMM) Boot(context.Context, vmm.BootSpec) ([]vmm.AttachedVolume, error) {
	return nil, nil
}
func (f *policyVMM) Stop(string, bool)                   {}
func (f *policyVMM) Destroy(string)                      {}
func (f *policyVMM) ConsoleLogPath(string) string        { return "" }
func (f *policyVMM) RootDiskPath(string) (string, error) { return "", nil }
func (f *policyVMM) Running() []vmm.RunningVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vmm.RunningVM(nil), f.running...)
}
func (f *policyVMM) ApplyACL(vmID string, u vmm.ACLUpdate) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, vm := range f.running {
		if vm.VMID == vmID {
			f.applied = append(f.applied, u)
			return true, f.fail
		}
	}
	return false, nil
}

// fakePolicyServer records what the agent sends and answers each
// Subscribe with whatever respond returns.
type fakePolicyServer struct {
	networkagentv1.UnimplementedPolicyDistributionServiceServer
	subs    chan *networkagentv1.Subscribe
	acks    chan *networkagentv1.Ack
	respond func(*networkagentv1.Subscribe) *networkagentv1.PolicyStreamResponse
}

func (s *fakePolicyServer) Stream(stream networkagentv1.PolicyDistributionService_StreamServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		switch m := req.GetMsg().(type) {
		case *networkagentv1.PolicyStreamRequest_Subscribe:
			s.subs <- m.Subscribe
			if r := s.respond(m.Subscribe); r != nil {
				if err := stream.Send(r); err != nil {
					return err
				}
			}
		case *networkagentv1.PolicyStreamRequest_Ack:
			s.acks <- m.Ack
		}
	}
}

func recv[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// TestPolicyStream: the agent subscribes to its wired interfaces, applies
// a policy through the driver holding the tap, hands set updates to SNAP,
// acks (or nacks on failure), and re-subscribes when what's wired changes.
func TestPolicyStream(t *testing.T) {
	dir := t.TempDir()
	plugin := filepath.Join(dir, "snap.sh")
	if err := os.WriteFile(plugin, []byte("#!/bin/sh\necho \"$1\" >> "+filepath.Join(dir, "verbs")+"\ncat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := &fakePolicyServer{subs: make(chan *networkagentv1.Subscribe, 10), acks: make(chan *networkagentv1.Ack, 10)}
	nonce := uint64(0)
	srv.respond = func(sub *networkagentv1.Subscribe) *networkagentv1.PolicyStreamResponse {
		var r networkagentv1.PolicyStreamResponse
		nonce++
		r.Nonce = nonce
		for _, id := range sub.GetInterfaceIds() {
			r.Interfaces = append(r.Interfaces, &networkagentv1.InterfacePolicy{
				IfaceId: id, VmId: "vm-" + id, IpAddress: "10.0.0.5", MacAddress: "02:00:00:00:00:05", Version: 10,
				Policy: &networkv1.SecurityPolicy{IngressRules: []*networkv1.SecurityPolicyRule{{Set: "sg:x"}}},
			})
		}
		r.Sets = []*networkagentv1.SetUpdate{{Name: "sg:x", Version: 10, Full: true, Members: []string{"10.0.0.5"}}}
		return &r
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	networkagentv1.RegisterPolicyDistributionServiceServer(gs, srv)
	go gs.Serve(lis)
	defer gs.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	driver := &policyVMM{running: []vmm.RunningVM{{VMID: "vm-a", NetworkInterfaces: []string{"a"}}}}
	agent := &Agent{Hypervisor: "hv-1", Drivers: map[string]vmm.VMM{"FIRECRACKER": driver}, SecurityBackendBin: plugin,
		PolicyClient: networkagentv1.NewPolicyDistributionServiceClient(conn), policyKick: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agent.runPolicyStream(ctx)

	if sub := recv(t, srv.subs, "first Subscribe"); sub.GetHypervisor() != "hv-1" || len(sub.GetInterfaceIds()) != 1 || sub.GetInterfaceIds()[0] != "a" {
		t.Fatalf("first Subscribe = %+v", sub)
	}
	if ack := recv(t, srv.acks, "ack"); ack.GetNonce() != 1 || ack.GetError() != "" {
		t.Fatalf("ack = %+v", ack)
	}
	driver.mu.Lock()
	if len(driver.applied) != 1 || driver.applied[0].IfaceID != "a" || driver.applied[0].IPAddress != "10.0.0.5" || driver.applied[0].Policy.IngressRules[0].Set != "sg:x" {
		t.Fatalf("applied = %+v", driver.applied)
	}
	driver.mu.Unlock()
	// (the fake driver stands in for the attach; the sets go to SNAP directly)
	if verbs, _ := os.ReadFile(filepath.Join(dir, "verbs")); string(verbs) != "update_sets\n" {
		t.Fatalf("SNAP calls = %q, want update_sets", verbs)
	}

	// A newly wired VM (whose ApplyACL now fails) triggers a re-subscribe
	// with both interfaces, answered with a NACK.
	driver.mu.Lock()
	driver.running = append(driver.running, vmm.RunningVM{VMID: "vm-b", NetworkInterfaces: []string{"b"}})
	driver.fail = errors.New("boom")
	driver.mu.Unlock()
	agent.kickPolicyStream()
	if sub := recv(t, srv.subs, "second Subscribe"); len(sub.GetInterfaceIds()) != 2 {
		t.Fatalf("second Subscribe = %+v", sub)
	}
	if ack := recv(t, srv.acks, "nack"); ack.GetNonce() != 2 || ack.GetError() == "" {
		t.Fatalf("nack = %+v", ack)
	}
}

func TestAgent_ShouldApplyACL(t *testing.T) {
	a := &Agent{}
	if !a.shouldApplyACL("iface-1", 5) {
		t.Fatal("first policy for an interface must apply")
	}
	a.recordAppliedACL("iface-1", 5)
	if !a.shouldApplyACL("iface-1", 5) {
		t.Fatal("a resend of the same version is harmless and must apply")
	}
	if a.shouldApplyACL("iface-1", 3) {
		t.Fatal("an older version must not apply")
	}
	if !a.shouldApplyACL("iface-2", 1) {
		t.Fatal("interfaces are independent")
	}
}
