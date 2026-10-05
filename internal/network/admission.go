package network

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
)

// admissionSubnetSpec/admissionNetworkInterfaceSpec (and the Status
// variants) are the snake_case JSON shapes sent to admission webhooks --
// explicitly tagged mirrors rather than marshaling the Go structs, which
// have no json tags, same reasoning as compute's admissionVMSpec.
type admissionAddress struct {
	CIDR      string `json:"cidr"`
	GatewayIP string `json:"gateway_ip,omitempty"`
}

type admissionSubnetSpec struct {
	NetworkID           string             `json:"network_id"`
	Zone                string             `json:"zone"`
	RequestedAddresses  []admissionAddress `json:"requested_addresses,omitempty"`
	DNSServers          []string           `json:"dns_servers,omitempty"`
	AllocatableIPRanges []string           `json:"allocatable_ip_ranges,omitempty"`
}

type admissionSubnetStatus struct {
	Phase      string             `json:"phase"`
	Addresses  []admissionAddress `json:"addresses,omitempty"`
	Values     map[string]int64   `json:"values,omitempty"`
	Attributes map[string]string  `json:"attributes,omitempty"`
}

type admissionNetworkSpec struct {
	NetworkClass        string   `json:"network_class"`
	DNSSuffix           string   `json:"dns_suffix,omitempty"`
	Visibility          string   `json:"visibility,omitempty"`
	SharedWithTenantIDs []string `json:"shared_with_tenant_ids,omitempty"`
}

type admissionNetworkStatus struct {
	Phase      string            `json:"phase"`
	Values     map[string]int64  `json:"values,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type admissionPoolRef struct {
	PoolID string `json:"pool_id"`
	Name   string `json:"name,omitempty"`
}

type admissionClassSpec struct {
	Network               []admissionPoolRef            `json:"network,omitempty"`
	Subnet                map[string][]admissionPoolRef `json:"subnet,omitempty"`
	Attributes            map[string]string             `json:"attributes,omitempty"`
	Visibility            string                        `json:"visibility,omitempty"`
	SharedWithTenantIDs   []string                      `json:"shared_with_tenant_ids,omitempty"`
	AllowPublicNetworks   bool                          `json:"allow_public_networks,omitempty"`
	DefaultDNSServers     map[string][]string           `json:"default_dns_servers,omitempty"`
	MTU                   int32                         `json:"mtu,omitempty"`
	GatewayPlacement      string                        `json:"gateway_placement,omitempty"`
	HostAggregateSelector map[string]string             `json:"host_aggregate_selector,omitempty"`
}

type admissionNetworkInterfaceSpec struct {
	VMID             string   `json:"vm_id"`
	SubnetID         string   `json:"subnet_id,omitempty"`
	NetworkID        string   `json:"network_id,omitempty"`
	Zone             string   `json:"zone,omitempty"`
	SecurityGroupIDs []string `json:"security_group_ids,omitempty"`
}

type admissionSecurityGroupPeer struct {
	CIDR            string `json:"cidr,omitempty"`
	SecurityGroupID string `json:"security_group_id,omitempty"`
	NetworkID       string `json:"network_id,omitempty"`
}

type admissionSecurityGroupRule struct {
	Protocol    string                     `json:"protocol,omitempty"`
	PortRange   string                     `json:"port_range,omitempty"`
	Peer        admissionSecurityGroupPeer `json:"peer"`
	Description string                     `json:"description,omitempty"`
}

type admissionSecurityGroupSpec struct {
	Description         string                       `json:"description,omitempty"`
	IngressRules        []admissionSecurityGroupRule `json:"ingress_rules,omitempty"`
	EgressRules         []admissionSecurityGroupRule `json:"egress_rules,omitempty"`
	SharedWithTenantIDs []string                     `json:"shared_with_tenant_ids,omitempty"`
}

type admissionSecurityGroupStatus struct {
	DefaultForNetworkID string `json:"default_for_network_id,omitempty"`
}

func toAdmissionSGRules(rules []SecurityGroupRule) []admissionSecurityGroupRule {
	var out []admissionSecurityGroupRule
	for _, r := range rules {
		out = append(out, admissionSecurityGroupRule{Protocol: r.Protocol, PortRange: r.PortRange, Peer: admissionSecurityGroupPeer(r.Peer), Description: r.Description})
	}
	return out
}

func admissionSecurityGroupSpecJSON(s SecurityGroupSpec) json.RawMessage {
	return mustJSON(admissionSecurityGroupSpec{
		Description: s.Description, IngressRules: toAdmissionSGRules(s.IngressRules), EgressRules: toAdmissionSGRules(s.EgressRules),
		SharedWithTenantIDs: s.SharedWithTenantIDs,
	})
}

func admissionSecurityGroupObject(g SecurityGroup) *admissionwebhook.Object {
	return &admissionwebhook.Object{
		ID: g.Meta.ID, Name: g.Meta.Name, TenantID: g.Meta.TenantID,
		Labels: g.Meta.Labels, Annotations: g.Meta.Annotations,
		Spec:   admissionSecurityGroupSpecJSON(g.Spec),
		Status: mustJSON(admissionSecurityGroupStatus{DefaultForNetworkID: g.Status.DefaultForNetworkID}),
	}
}

type admissionNetworkInterfaceStatus struct {
	Phase      string `json:"phase"`
	IPAddress  string `json:"ip_address,omitempty"`
	MACAddress string `json:"mac_address,omitempty"`
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("network: marshal admission payload: %v", err)) // plain structs of strings/ints: cannot fail
	}
	return b
}

func toAdmissionAddresses(as []SubnetAddress) []admissionAddress {
	var out []admissionAddress
	for _, a := range as {
		out = append(out, admissionAddress{CIDR: a.CIDR, GatewayIP: a.GatewayIP})
	}
	return out
}

func admissionSubnetSpecJSON(s SubnetSpec) json.RawMessage {
	return mustJSON(admissionSubnetSpec{
		NetworkID: s.NetworkID, Zone: s.Zone, RequestedAddresses: toAdmissionAddresses(s.RequestedAddresses),
		DNSServers: s.DNSServers, AllocatableIPRanges: s.AllocatableIPRanges,
	})
}

func admissionNetworkSpecJSON(s NetworkSpec) json.RawMessage {
	return mustJSON(admissionNetworkSpec{NetworkClass: s.NetworkClass, DNSSuffix: s.DNSSuffix, Visibility: string(s.Visibility), SharedWithTenantIDs: s.SharedWithTenantIDs})
}

func toAdmissionRefs(refs []PoolRef) []admissionPoolRef {
	var out []admissionPoolRef
	for _, r := range refs {
		out = append(out, admissionPoolRef{PoolID: r.PoolID, Name: r.Name})
	}
	return out
}

func admissionClassSpecJSON(c NetworkClassSpec) json.RawMessage {
	subnet := map[string][]admissionPoolRef{}
	for z, refs := range c.Subnet {
		subnet[z] = toAdmissionRefs(refs)
	}
	return mustJSON(admissionClassSpec{
		Network: toAdmissionRefs(c.Network), Subnet: subnet, Attributes: c.Attributes,
		Visibility: string(c.Visibility), SharedWithTenantIDs: c.SharedWithTenantIDs, AllowPublicNetworks: c.AllowPublicNetworks,
		DefaultDNSServers: c.DefaultDNSServers, MTU: c.MTU, GatewayPlacement: string(c.GatewayPlacement), HostAggregateSelector: c.HostAggregateSelector,
	})
}

func admissionNetworkObject(n Network) *admissionwebhook.Object {
	return &admissionwebhook.Object{
		ID: n.Meta.ID, Name: n.Meta.Name, TenantID: n.Meta.TenantID,
		Labels: n.Meta.Labels, Annotations: n.Meta.Annotations,
		Spec:   admissionNetworkSpecJSON(n.Spec),
		Status: mustJSON(admissionNetworkStatus{Phase: string(n.Status.Phase), Values: n.Status.Values, Attributes: n.Status.Attributes}),
	}
}

func admissionClassObject(c NetworkClass) *admissionwebhook.Object {
	return &admissionwebhook.Object{
		ID: c.Meta.ID, Name: c.Meta.Name,
		Labels: c.Meta.Labels, Annotations: c.Meta.Annotations,
		Spec: admissionClassSpecJSON(c.Spec),
	}
}

func admissionNetworkInterfaceSpecJSON(s NetworkInterfaceSpec) json.RawMessage {
	return mustJSON(admissionNetworkInterfaceSpec{
		VMID: s.VMID, SubnetID: s.SubnetID, NetworkID: s.NetworkID, Zone: s.Zone, SecurityGroupIDs: s.SecurityGroupIDs,
	})
}

func admissionSubnetObject(sn Subnet) *admissionwebhook.Object {
	return &admissionwebhook.Object{
		ID: sn.Meta.ID, Name: sn.Meta.Name, TenantID: sn.Meta.TenantID,
		Labels: sn.Meta.Labels, Annotations: sn.Meta.Annotations,
		Spec:   admissionSubnetSpecJSON(sn.Spec),
		Status: mustJSON(admissionSubnetStatus{Phase: string(sn.Status.Phase), Addresses: toAdmissionAddresses(sn.Status.Addresses), Values: sn.Status.Values, Attributes: sn.Status.Attributes}),
	}
}

func admissionNetworkInterfaceObject(n NetworkInterface) *admissionwebhook.Object {
	return &admissionwebhook.Object{
		ID: n.Meta.ID, Name: n.Meta.Name, TenantID: n.Meta.TenantID,
		Labels: n.Meta.Labels, Annotations: n.Meta.Annotations,
		Spec: admissionNetworkInterfaceSpecJSON(n.Spec),
		Status: mustJSON(admissionNetworkInterfaceStatus{
			Phase: string(n.Status.Phase), IPAddress: n.Status.IPAddress, MACAddress: n.Status.MACAddress,
		}),
	}
}

// admit consults s.AdmissionGate (a no-op when no webhook URL is
// configured) as the last check before a write, mapping its outcome onto
// ErrAdmissionDenied/ErrAdmissionUnavailable -- see docs/specs/
// external-integration.md「ゲート系(作成側)」.
func (s *Service) admit(ctx context.Context, req admissionwebhook.Request) error {
	allowed, reason, err := s.AdmissionGate.Validate(ctx, req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAdmissionUnavailable, err)
	}
	if !allowed {
		return fmt.Errorf("%w: %s", ErrAdmissionDenied, reason)
	}
	return nil
}
