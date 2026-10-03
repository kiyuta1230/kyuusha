// Package snap implements SNAP (Security Network Attach Protocol), a
// pluggable ACL/security-backend contract for a VM's NetworkInterface,
// deliberately separate from netsetup's VNAP (VM Network Attach Protocol,
// tap-to-switch wiring) contract even though the mechanics are copy-pasted
// from it (exec, stdin JSON, exit-code-only success, 10s timeout, plugin-
// side idempotency) -- wiring and ACL enforcement are orthogonal concerns
// (an operator may want to swap one without the other, e.g. keep the
// built-in Linux bridge but enforce ACLs with eBPF or OVS instead of
// nftables), so they get independent flags/binaries rather than one
// combined plugin. See docs/specs/snap.md for the full contract and
// docs/specs/vnap.md for VNAP's own identical-shape precedent this
// mirrors.
package snap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/nftacl"
)

// FirewallRule mirrors network.FirewallRule/vmm.FirewallRule -- its own
// copy, not an import, same "each layer has its own mirror struct"
// convention netsetup.Interface itself already follows for the wiring
// side.
type FirewallRule struct {
	Protocol   string
	PortRange  string
	SourceCIDR string
	Action     string
}

// Interface is everything Attach/Detach need for one VM network
// attachment's ACL enforcement, mirroring netsetup.Interface's own shape.
type Interface struct {
	IfaceID      string
	VMID         string
	TenantID     string
	TapName      string
	SubnetID     string
	SubnetLabels map[string]string
	SubnetCIDR   string
	GatewayIP    string
	// IPAddress/MACAddress are the VM's own allocated address on this
	// interface: the anti-spoofing input (see docs/specs/snap.md).
	IPAddress  string
	MACAddress string

	IngressRules []FirewallRule
	EgressRules  []FirewallRule
}

// Attach applies iface's ingress/egress rules: the built-in nftacl
// implementation when securityBackendBin is empty (the default -- see
// nftacl's own doc comment), or an external plugin otherwise. Idempotent
// either way: safe to call again for a tap whose rules haven't changed, or
// to re-apply a fresh rule set (see Attach's callers: both Boot's initial
// wiring and a later UpdateFirewallRules-triggered re-apply call this the
// same way).
func Attach(iface Interface, securityBackendBin string) error {
	if securityBackendBin == "" {
		return nftacl.Apply(nftacl.Interface{
			TapName:      iface.TapName,
			SubnetCIDR:   iface.SubnetCIDR,
			GatewayIP:    iface.GatewayIP,
			IPAddress:    iface.IPAddress,
			MACAddress:   iface.MACAddress,
			IngressRules: toNftaclRules(iface.IngressRules),
			EgressRules:  toNftaclRules(iface.EgressRules),
		})
	}
	return runPlugin(securityBackendBin, "attach", pluginRequest{
		TapName: iface.TapName, IfaceID: iface.IfaceID, VMID: iface.VMID, TenantID: iface.TenantID,
		SubnetID: iface.SubnetID, SubnetLabels: iface.SubnetLabels, SubnetCIDR: iface.SubnetCIDR, GatewayIP: iface.GatewayIP,
		IPAddress: iface.IPAddress, MACAddress: iface.MACAddress,
		IngressRules: toPluginRules(iface.IngressRules), EgressRules: toPluginRules(iface.EgressRules),
	})
}

// Detach removes whatever ACL state Attach installed for tapName. The tap
// device itself is untouched (netsetup.DeleteTap's own job) -- Detach only
// undoes what Attach did. Best-effort from the caller's perspective, same
// as netsetup.DeleteTap: a Detach failure must never block tap removal.
func Detach(ifaceID, vmID, tenantID, tapName, securityBackendBin string) error {
	if securityBackendBin == "" {
		return nftacl.Remove(tapName)
	}
	return runPlugin(securityBackendBin, "detach", pluginRequest{
		TapName: tapName, IfaceID: ifaceID, VMID: vmID, TenantID: tenantID,
	})
}

func toNftaclRules(rules []FirewallRule) []nftacl.FirewallRule {
	var out []nftacl.FirewallRule
	for _, r := range rules {
		out = append(out, nftacl.FirewallRule{Protocol: r.Protocol, PortRange: r.PortRange, SourceCIDR: r.SourceCIDR, Action: r.Action})
	}
	return out
}

// pluginRequest is the JSON an external security-backend plugin receives on
// stdin. Detach only ever sets TapName/IfaceID/VMID/TenantID (omitempty
// drops the rest), same reasoning as netsetup's own pluginRequest: removing
// a port never needs to know what it used to be configured with.
type pluginRequest struct {
	TapName      string            `json:"tap_name"`
	IfaceID      string            `json:"iface_id"`
	VMID         string            `json:"vm_id"`
	TenantID     string            `json:"tenant_id"`
	SubnetID     string            `json:"subnet_id,omitempty"`
	SubnetLabels map[string]string `json:"subnet_labels,omitempty"`
	SubnetCIDR   string            `json:"subnet_cidr,omitempty"`
	GatewayIP    string            `json:"gateway_ip,omitempty"`
	IPAddress    string            `json:"ip_address,omitempty"`
	MACAddress   string            `json:"mac_address,omitempty"`

	IngressRules []pluginFirewallRule `json:"ingress_rules,omitempty"`
	EgressRules  []pluginFirewallRule `json:"egress_rules,omitempty"`
}

type pluginFirewallRule struct {
	Protocol   string `json:"protocol"`
	PortRange  string `json:"port_range,omitempty"`
	SourceCIDR string `json:"source_cidr"`
	Action     string `json:"action"`
}

func toPluginRules(rules []FirewallRule) []pluginFirewallRule {
	var out []pluginFirewallRule
	for _, r := range rules {
		out = append(out, pluginFirewallRule{Protocol: r.Protocol, PortRange: r.PortRange, SourceCIDR: r.SourceCIDR, Action: r.Action})
	}
	return out
}

// pluginTimeout mirrors netsetup's own pluginTimeout constant exactly: a
// hung security-backend plugin must not hang VM boot/teardown/ACL-update
// indefinitely.
const pluginTimeout = 10 * time.Second

// runPlugin mirrors netsetup.runPlugin's exec/stdin-JSON/exit-code contract
// verbatim -- see this package's own doc comment for why it's a separate
// contract (separate flag/binary) rather than reusing netsetup's.
func runPlugin(securityBackendBin, verb string, req pluginRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("snap: marshal request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityBackendBin, verb)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("snap: security-backend plugin %s %s: %w: %s", securityBackendBin, verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}
