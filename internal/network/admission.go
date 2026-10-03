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
type admissionSubnetSpec struct {
	Zone                string   `json:"zone"`
	CIDR                string   `json:"cidr"`
	GatewayIP           string   `json:"gateway_ip,omitempty"`
	DNSServers          []string `json:"dns_servers,omitempty"`
	DNSSuffix           string   `json:"dns_suffix,omitempty"`
	MeshGroup           string   `json:"mesh_group,omitempty"`
	AllocatableIPRanges []string `json:"allocatable_ip_ranges,omitempty"`
	UniqueCidr          bool     `json:"unique_cidr,omitempty"`
	Visibility          string   `json:"visibility,omitempty"`
	SharedWithTenantIDs []string `json:"shared_with_tenant_ids,omitempty"`
}

type admissionSubnetStatus struct {
	Phase  string `json:"phase"`
	VLANID int32  `json:"vlan_id,omitempty"`
}

type admissionNetworkInterfaceSpec struct {
	VMID         string             `json:"vm_id"`
	SubnetID     string             `json:"subnet_id"`
	IngressRules []FirewallRuleInfo `json:"ingress_rules,omitempty"`
	EgressRules  []FirewallRuleInfo `json:"egress_rules,omitempty"`
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

func admissionSubnetSpecJSON(s SubnetSpec) json.RawMessage {
	return mustJSON(admissionSubnetSpec{
		Zone: s.Zone, CIDR: s.CIDR, GatewayIP: s.GatewayIP, DNSServers: s.DNSServers, DNSSuffix: s.DNSSuffix,
		MeshGroup: s.MeshGroup, AllocatableIPRanges: s.AllocatableIPRanges, UniqueCidr: s.UniqueCidr,
		Visibility: string(s.Visibility), SharedWithTenantIDs: s.SharedWithTenantIDs,
	})
}

func admissionNetworkInterfaceSpecJSON(s NetworkInterfaceSpec) json.RawMessage {
	return mustJSON(admissionNetworkInterfaceSpec{
		VMID: s.VMID, SubnetID: s.SubnetID,
		IngressRules: toFirewallRuleInfos(s.IngressRules), EgressRules: toFirewallRuleInfos(s.EgressRules),
	})
}

func admissionSubnetObject(sn Subnet) *admissionwebhook.Object {
	return &admissionwebhook.Object{
		ID: sn.Meta.ID, Name: sn.Meta.Name, TenantID: sn.Meta.TenantID,
		Labels: sn.Meta.Labels, Annotations: sn.Meta.Annotations,
		Spec:   admissionSubnetSpecJSON(sn.Spec),
		Status: mustJSON(admissionSubnetStatus{Phase: string(sn.Status.Phase), VLANID: sn.Status.VLANID}),
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
