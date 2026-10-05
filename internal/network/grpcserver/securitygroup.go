package grpcserver

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
	"github.com/kiyuta1230/kyuusha/internal/network"
	"github.com/kiyuta1230/kyuusha/internal/resource"
)

type SecurityGroupServer struct {
	networkv1.UnimplementedSecurityGroupServiceServer
	svc *network.Service
}

func NewSecurityGroupServer(svc *network.Service) *SecurityGroupServer {
	return &SecurityGroupServer{svc: svc}
}

func (s *SecurityGroupServer) Create(ctx context.Context, req *networkv1.CreateSecurityGroupRequest) (*networkv1.SecurityGroup, error) {
	g, err := s.svc.CreateSecurityGroup(ctx, req.GetTenantId(), req.GetName(), fromSecurityGroupSpec(req.GetSpec()),
		resource.Metadata{Labels: req.GetLabels(), Annotations: req.GetAnnotations()})
	if err != nil {
		return nil, toStatus(err)
	}
	return toSecurityGroup(*g), nil
}

func (s *SecurityGroupServer) Get(ctx context.Context, req *networkv1.GetSecurityGroupRequest) (*networkv1.SecurityGroup, error) {
	g, err := s.svc.GetSecurityGroup(ctx, req.GetTenantId(), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toSecurityGroup(*g), nil
}

func (s *SecurityGroupServer) List(ctx context.Context, req *networkv1.ListSecurityGroupsRequest) (*networkv1.ListSecurityGroupsResponse, error) {
	gs, err := s.svc.ListSecurityGroups(ctx, req.GetTenantId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListSecurityGroupsResponse{}
	for _, g := range gs {
		out.Items = append(out.Items, toSecurityGroup(g))
	}
	return out, nil
}

func (s *SecurityGroupServer) Update(ctx context.Context, req *networkv1.UpdateSecurityGroupRequest) (*networkv1.SecurityGroup, error) {
	in := req.GetSecurityGroup()
	g := network.SecurityGroup{Meta: fromMetaProto(in.GetMeta()), Spec: fromSecurityGroupSpec(in.GetSpec())}
	out, err := s.svc.UpdateSecurityGroup(ctx, req.GetTenantId(), &g)
	if err != nil {
		return nil, toStatus(err)
	}
	return toSecurityGroup(*out), nil
}

func (s *SecurityGroupServer) Delete(ctx context.Context, req *networkv1.DeleteSecurityGroupRequest) (*emptypb.Empty, error) {
	if err := s.svc.DeleteSecurityGroup(ctx, req.GetTenantId(), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *SecurityGroupServer) Watch(req *networkv1.WatchSecurityGroupsRequest, stream networkv1.SecurityGroupService_WatchServer) error {
	events, err := s.svc.WatchSecurityGroups(stream.Context(), req.GetTenantId(), req.GetSinceResourceVersion())
	if err != nil {
		return toStatus(err)
	}
	for e := range events {
		out := &networkv1.SecurityGroupEvent{Type: networkv1.SecurityGroupEvent_Type(eventType(e.Type)), ResourceVersion: e.ResourceVersion}
		if e.Type != network.EventBookmark {
			out.SecurityGroup = toSecurityGroup(e.Object)
		}
		if err := stream.Send(out); err != nil {
			return err
		}
	}
	return stream.Context().Err()
}

func fromSGRules(rs []*networkv1.SecurityGroupRule) []network.SecurityGroupRule {
	var out []network.SecurityGroupRule
	for _, r := range rs {
		p := r.GetPeer()
		out = append(out, network.SecurityGroupRule{
			Protocol: r.GetProtocol(), PortRange: r.GetPortRange(), Description: r.GetDescription(),
			Peer: network.SecurityGroupPeer{CIDR: p.GetCidr(), SecurityGroupID: p.GetSecurityGroupId(), NetworkID: p.GetNetworkId()},
		})
	}
	return out
}

func toSGRules(rs []network.SecurityGroupRule) []*networkv1.SecurityGroupRule {
	var out []*networkv1.SecurityGroupRule
	for _, r := range rs {
		peer := &networkv1.SecurityGroupPeer{}
		switch {
		case r.Peer.CIDR != "":
			peer.Peer = &networkv1.SecurityGroupPeer_Cidr{Cidr: r.Peer.CIDR}
		case r.Peer.SecurityGroupID != "":
			peer.Peer = &networkv1.SecurityGroupPeer_SecurityGroupId{SecurityGroupId: r.Peer.SecurityGroupID}
		case r.Peer.NetworkID != "":
			peer.Peer = &networkv1.SecurityGroupPeer_NetworkId{NetworkId: r.Peer.NetworkID}
		}
		out = append(out, &networkv1.SecurityGroupRule{Protocol: r.Protocol, PortRange: r.PortRange, Peer: peer, Description: r.Description})
	}
	return out
}

func fromSecurityGroupSpec(s *networkv1.SecurityGroupSpec) network.SecurityGroupSpec {
	return network.SecurityGroupSpec{
		Description: s.GetDescription(), IngressRules: fromSGRules(s.GetIngressRules()), EgressRules: fromSGRules(s.GetEgressRules()),
		SharedWithTenantIDs: s.GetSharedWithTenantIds(),
	}
}

func toSecurityGroup(g network.SecurityGroup) *networkv1.SecurityGroup {
	return &networkv1.SecurityGroup{
		Meta: toMetaProto(g.Meta),
		Spec: &networkv1.SecurityGroupSpec{
			Description: g.Spec.Description, IngressRules: toSGRules(g.Spec.IngressRules), EgressRules: toSGRules(g.Spec.EgressRules),
			SharedWithTenantIds: g.Spec.SharedWithTenantIDs,
		},
		Status: &networkv1.SecurityGroupStatus{DefaultForNetworkId: g.Status.DefaultForNetworkID},
	}
}
