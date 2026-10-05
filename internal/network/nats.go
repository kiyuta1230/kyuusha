package network

import (
	"context"
	"fmt"
	"time"

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

// CmdSubjectUpdateACL is where the hypervisor currently running a
// NetworkInterface's VM is told the interface's SecurityPolicy changed (its
// groups, or a group's rules) -- see internal/compute-agent/snap and
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

// PolicyRuleInfo/AddressSetInfo/SecurityPolicyInfo are SecurityPolicy's
// JSON mirrors (same "mirror type, no dependency on the proto package"
// convention as vmm.NetIface elsewhere) -- the wire shape docs/specs/snap.md
// documents.
type PolicyRuleInfo struct {
	Protocol  string `json:"protocol,omitempty"`
	PortRange string `json:"port_range,omitempty"`
	CIDR      string `json:"cidr,omitempty"`
	Set       string `json:"set,omitempty"`
}

type AddressSetInfo struct {
	Name    string   `json:"name"`
	Version int64    `json:"version"`
	Members []string `json:"members"`
}

type SecurityPolicyInfo struct {
	SecurityGroupIDs []string         `json:"security_group_ids,omitempty"`
	IngressRules     []PolicyRuleInfo `json:"ingress_rules,omitempty"`
	EgressRules      []PolicyRuleInfo `json:"egress_rules,omitempty"`
	Sets             []AddressSetInfo `json:"sets,omitempty"`
}

// ToInfo converts p to its wire shape.
func (p SecurityPolicy) ToInfo() SecurityPolicyInfo {
	out := SecurityPolicyInfo{SecurityGroupIDs: p.SecurityGroupIDs}
	for _, r := range p.IngressRules {
		out.IngressRules = append(out.IngressRules, PolicyRuleInfo(r))
	}
	for _, r := range p.EgressRules {
		out.EgressRules = append(out.EgressRules, PolicyRuleInfo(r))
	}
	for _, a := range p.Sets {
		out.Sets = append(out.Sets, AddressSetInfo{Name: a.Name, Version: a.Version, Members: a.Members})
	}
	return out
}

// CmdSubjectUpdateSets is where network-reconciler sends a Hypervisor the
// membership changes of the address sets its interfaces' rules reference
// (see sgsync.go and docs/specs/snap.md).
func CmdSubjectUpdateSets(hypervisor string) string {
	return fmt.Sprintf("ms.network.cmd.%s.security_group.update_sets", hypervisor)
}

// SetUpdate changes one address set: Full replaces its members with
// Members; otherwise Add/Remove are applied. Version is the etcd revision
// the change reflects -- a host ignores anything not newer than what it
// last applied to that set.
type SetUpdate struct {
	Name    string   `json:"name"`
	Version int64    `json:"version"`
	Full    bool     `json:"full,omitempty"`
	Members []string `json:"members,omitempty"`
	Add     []string `json:"add,omitempty"`
	Remove  []string `json:"remove,omitempty"`
}

// UpdateSetsCommand is CmdSubjectUpdateSets' payload. ObservedAt is when
// network-reconciler saw the change, for the host's propagation-delay
// metric.
type UpdateSetsCommand struct {
	Sets       []SetUpdate `json:"sets"`
	ObservedAt time.Time   `json:"observed_at"`
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

// UpdateACLCommand is CmdSubjectUpdateACL's payload: the interface's
// complete current SecurityPolicy (never a delta), so whichever copy a
// compute-agent ends up applying (including a stale, redelivered one --
// see ResourceVersion) converges to a valid state.
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

	Policy SecurityPolicyInfo `json:"policy"`

	// ResourceVersion lets a compute-agent reject a stale/out-of-order
	// redelivery (e.g. two SetSecurityGroups calls racing) rather than
	// clobber a newer already-applied state with an older one -- see
	// internal/compute-agent/agent.go's handleUpdateACL.
	ResourceVersion int64 `json:"resource_version"`
}
