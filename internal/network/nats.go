package network

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// Subject naming follows internal/compute/nats.go's own convention
// (docs/architecture.md "NATS JetStream: subject/stream設計"):
// ms.<service>.<cmd|evt>.<hypervisor>.<resource-type>.<verb>. This is
// network's first NATS involvement at all -- see cmd/network's package doc
// comment for why a "stateless API binary" dialing NATS/compute doesn't
// break its statelessness (same exception cmd/compute already documents for
// itself).
const cmdStreamName = "NETWORK_CMD"

// CmdSubjectUpdateACL is where UpdateFirewallRules notifies the hypervisor
// currently running a NetworkInterface's VM that its ingress_rules/
// egress_rules changed -- see internal/compute-agent/snap and
// docs/specs/snap.md.
func CmdSubjectUpdateACL(hypervisor string) string {
	return fmt.Sprintf("ms.network.cmd.%s.network_interface.update_acl", hypervisor)
}

// EnsureStreams is idempotent (CreateOrUpdateStream) and safe to call from
// both cmd/network and compute-agent at startup, same as
// compute.EnsureStreams.
func EnsureStreams(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cmdStreamName,
		Subjects:  []string{"ms.network.cmd.>"},
		Retention: jetstream.WorkQueuePolicy,
	})
	if err != nil {
		return fmt.Errorf("ensure %s stream: %w", cmdStreamName, err)
	}
	return nil
}

// FirewallRuleInfo is UpdateACLCommand's JSON-shaped mirror of FirewallRule
// (same "mirror type, no dependency on the proto package" convention as
// vmm.NetIface elsewhere in this codebase).
type FirewallRuleInfo struct {
	Protocol   string `json:"protocol"`
	PortRange  string `json:"port_range,omitempty"`
	SourceCIDR string `json:"source_cidr"`
	Action     string `json:"action"`
}

func toFirewallRuleInfos(rules []FirewallRule) []FirewallRuleInfo {
	if len(rules) == 0 {
		return nil
	}
	out := make([]FirewallRuleInfo, len(rules))
	for i, r := range rules {
		out[i] = FirewallRuleInfo{Protocol: r.Protocol, PortRange: r.PortRange, SourceCIDR: r.SourceCIDR, Action: r.Action}
	}
	return out
}

// AttachContext is everything about a NetworkInterface's Network,
// NetworkClass and Subnet allocation a VNAP/SNAP plugin is handed, beyond
// the interface's own address -- see docs/specs/vnap.md. compute builds the
// same thing for boot (internal/compute's NetworkInterfaceInfo), this
// package for update_acl.
type AttachContext struct {
	NetworkID              string            `json:"network_id,omitempty"`
	NetworkLabels          map[string]string `json:"network_labels,omitempty"`
	NetworkClass           string            `json:"network_class,omitempty"` // the class's name
	NetworkClassAttributes map[string]string `json:"network_class_attributes,omitempty"`
	NetworkValues          map[string]int64  `json:"network_values,omitempty"`
	NetworkAttributes      map[string]string `json:"network_attributes,omitempty"`
	SubnetValues           map[string]int64  `json:"subnet_values,omitempty"`
	SubnetAttributes       map[string]string `json:"subnet_attributes,omitempty"`
	MTU                    int32             `json:"mtu,omitempty"`
}

// UpdateACLCommand is CmdSubjectUpdateACL's payload. Always carries the
// interface's *complete* current rule sets (never a delta) -- so whichever
// copy a compute-agent ends up applying (including a stale, redelivered
// one -- see ResourceVersion below) converges to a valid state, never a
// partial one.
type UpdateACLCommand struct {
	IfaceID      string            `json:"iface_id"`
	VMID         string            `json:"vm_id"`
	TenantID     string            `json:"tenant_id"`
	SubnetID     string            `json:"subnet_id,omitempty"`
	SubnetLabels map[string]string `json:"subnet_labels,omitempty"`
	SubnetCIDR   string            `json:"subnet_cidr"`
	GatewayIP    string            `json:"gateway_ip,omitempty"`
	// IPAddress/MACAddress are the interface's own allocated address,
	// carried so a re-apply can rebuild the anti-spoofing checks Boot
	// installed (see docs/specs/snap.md) without compute-agent persisting
	// them itself.
	IPAddress  string `json:"ip_address,omitempty"`
	MACAddress string `json:"mac_address,omitempty"`
	// Attach carries the interface's Network/NetworkClass context, same
	// fields compute hands compute-agent at boot (see AttachContext).
	Attach AttachContext `json:"attach"`

	IngressRules []FirewallRuleInfo `json:"ingress_rules,omitempty"`
	EgressRules  []FirewallRuleInfo `json:"egress_rules,omitempty"`

	// ResourceVersion lets a compute-agent reject a stale/out-of-order
	// redelivery (e.g. two UpdateFirewallRules calls racing) rather than
	// clobber a newer already-applied state with an older one -- see
	// internal/compute-agent/agent.go's handleUpdateACL.
	ResourceVersion int64 `json:"resource_version"`
}
