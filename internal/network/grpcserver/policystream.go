package grpcserver

import (
	"io"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	networkagentv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/agent/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
	"github.com/kiyuta1230/kyuusha/internal/network"
)

// PolicyDistributionServer serves compute-agents' policy streams from a
// network.PolicyHub (see its doc comment). East-west only: the agent
// dials network directly over mTLS, never through api-gateway.
type PolicyDistributionServer struct {
	networkagentv1.UnimplementedPolicyDistributionServiceServer
	hub *network.PolicyHub
}

func NewPolicyDistributionServer(hub *network.PolicyHub) *PolicyDistributionServer {
	return &PolicyDistributionServer{hub: hub}
}

func (s *PolicyDistributionServer) Stream(stream networkagentv1.PolicyDistributionService_StreamServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	sub := first.GetSubscribe()
	if sub == nil || sub.GetHypervisor() == "" {
		return status.Error(codes.InvalidArgument, "the first message must be a Subscribe naming the hypervisor")
	}
	ps := s.hub.Connect(sub.GetHypervisor())
	defer ps.Close()

	// nonce -> revision of what was sent, for the Ack bookkeeping.
	var mu sync.Mutex
	sent := map[uint64]int64{}

	recvErr := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			switch m := req.GetMsg().(type) {
			case *networkagentv1.PolicyStreamRequest_Subscribe:
				ps.Subscribe(m.Subscribe.GetInterfaceIds())
			case *networkagentv1.PolicyStreamRequest_Ack:
				mu.Lock()
				rev, ok := sent[m.Ack.GetNonce()]
				for n := range sent {
					if n <= m.Ack.GetNonce() {
						delete(sent, n)
					}
				}
				mu.Unlock()
				if ok {
					ps.Ack(rev, m.Ack.GetError())
				}
			}
		}
	}()
	ps.Subscribe(sub.GetInterfaceIds())

	var nonce uint64
	for {
		select {
		case err := <-recvErr:
			if err == io.EOF {
				return nil
			}
			return err
		case <-ps.Dropped():
			return status.Error(codes.Unavailable, "policy stream fell too far behind; reconnect")
		case <-stream.Context().Done():
			return stream.Context().Err()
		case msg := <-ps.Messages():
			nonce++
			mu.Lock()
			sent[nonce] = msg.Revision
			mu.Unlock()
			if err := stream.Send(toPolicyResponse(nonce, msg)); err != nil {
				return err
			}
		}
	}
}

func toPolicyResponse(nonce uint64, m network.PolicyMessage) *networkagentv1.PolicyStreamResponse {
	out := &networkagentv1.PolicyStreamResponse{Nonce: nonce, Revision: m.Revision, ObservedAt: timestamppb.New(m.ObservedAt)}
	for _, p := range m.Interfaces {
		a := p.Attach
		out.Interfaces = append(out.Interfaces, &networkagentv1.InterfacePolicy{
			IfaceId: p.IfaceID, VmId: p.VMID, TenantId: p.TenantID, SubnetId: p.SubnetID, SubnetLabels: p.SubnetLabels,
			SubnetCidr: p.SubnetCIDR, GatewayIp: p.GatewayIP, IpAddress: p.IPAddress, MacAddress: p.MACAddress,
			Attach: &networkagentv1.AttachContext{
				NetworkId: a.NetworkID, NetworkLabels: a.NetworkLabels, NetworkClass: a.NetworkClass, NetworkClassAttributes: a.NetworkClassAttributes,
				NetworkValues: a.NetworkValues, NetworkAttributes: a.NetworkAttributes, SubnetValues: a.SubnetValues, SubnetAttributes: a.SubnetAttributes, Mtu: a.MTU,
			},
			Policy:  toSecurityPolicyProto(p.Policy),
			Version: p.Version,
		})
	}
	for _, u := range m.Sets {
		out.Sets = append(out.Sets, &networkagentv1.SetUpdate{Name: u.Name, Version: u.Version, Full: u.Full, Members: u.Members, Add: u.Add, Remove: u.Remove})
	}
	return out
}

func toSecurityPolicyProto(p network.SecurityPolicy) *networkv1.SecurityPolicy {
	out := &networkv1.SecurityPolicy{SecurityGroupIds: p.SecurityGroupIDs}
	conv := func(rs []network.PolicyRule) []*networkv1.SecurityPolicyRule {
		var o []*networkv1.SecurityPolicyRule
		for _, r := range rs {
			o = append(o, &networkv1.SecurityPolicyRule{Protocol: r.Protocol, PortRange: r.PortRange, Cidr: r.CIDR, Set: r.Set})
		}
		return o
	}
	out.IngressRules, out.EgressRules = conv(p.IngressRules), conv(p.EgressRules)
	for _, a := range p.Sets {
		out.Sets = append(out.Sets, &networkv1.AddressSet{Name: a.Name, Version: a.Version, Members: a.Members})
	}
	return out
}
