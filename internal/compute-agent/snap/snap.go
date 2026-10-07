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
	"sync"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/nftacl"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
)

// Interface is everything Attach/Detach need for one VM network
// attachment's ACL enforcement, mirroring netsetup.Interface's own shape.
type Interface struct {
	Attach       vmm.AttachInfo
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

	Policy vmm.SecurityPolicy
}

// setMu serializes everything that writes address sets (Attach,
// UpdateSets): the version check and the write must happen together, or a
// full copy checked as newest could land after a delta checked later.
var (
	setMu       sync.Mutex
	setVersions = map[string]int64{}
)

// freshSets drops every full copy in sets that's older than what this host
// already applied to that set, recording the rest. Caller holds setMu.
func freshSets(sets []vmm.SetUpdate) []vmm.SetUpdate {
	var out []vmm.SetUpdate
	for _, u := range sets {
		if u.Version < setVersions[u.Name] {
			continue
		}
		setVersions[u.Name] = u.Version
		u.Full = true
		out = append(out, u)
	}
	return out
}

// Attach applies iface's SecurityPolicy: the built-in nftacl
// implementation when securityBackendBin is empty (the default -- see
// nftacl's own doc comment), or an external plugin otherwise. Idempotent
// either way: Boot's initial wiring and a later policy-stream re-apply call
// it the same way. Set copies older than what the host already has
// (another interface's newer delta) are left out, so the backend keeps
// the newer contents.
func Attach(iface Interface, securityBackendBin string) error {
	setMu.Lock()
	defer setMu.Unlock()
	policy := iface.Policy
	policy.Sets = freshSets(policy.Sets)
	if securityBackendBin == "" {
		return nftacl.Apply(toNftaclInterface(iface.TapName, iface.GatewayIP, iface.IPAddress, iface.MACAddress, policy))
	}
	return runPlugin(securityBackendBin, "attach", PluginRequest{
		TapName: iface.TapName, IfaceID: iface.IfaceID, VMID: iface.VMID, TenantID: iface.TenantID,
		SubnetID: iface.SubnetID, SubnetLabels: iface.SubnetLabels, SubnetCIDR: iface.SubnetCIDR, GatewayIP: iface.GatewayIP,
		IPAddress: iface.IPAddress, MACAddress: iface.MACAddress, AttachInfo: iface.Attach,
		SecurityPolicy: policy,
	})
}

// UpdateSets applies address-set changes (network-reconciler's
// policy stream, see docs/specs/snap.md) and returns those actually passed
// on: a delta must be newer than what this host last applied to the set,
// a full copy at least as new. Sets no interface on this host references
// are the backend's to ignore.
func UpdateSets(updates []vmm.SetUpdate, securityBackendBin string) ([]vmm.SetUpdate, error) {
	setMu.Lock()
	defer setMu.Unlock()
	var fresh []vmm.SetUpdate
	for _, u := range updates {
		cur := setVersions[u.Name]
		if u.Version < cur || !u.Full && u.Version == cur {
			continue
		}
		setVersions[u.Name] = u.Version
		fresh = append(fresh, u)
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	if securityBackendBin == "" {
		return fresh, nftacl.UpdateSets(toNftaclSets(fresh))
	}
	return fresh, runPlugin(securityBackendBin, "update_sets", PluginRequest{SecurityPolicy: vmm.SecurityPolicy{Sets: fresh}})
}

// Detach removes whatever ACL state Attach installed for tapName. The tap
// device itself is untouched (netsetup.DeleteTap's own job) -- Detach only
// undoes what Attach did. Best-effort from the caller's perspective, same
// as netsetup.DeleteTap: a Detach failure must never block tap removal.
func Detach(ifaceID, vmID, tenantID, tapName, securityBackendBin string) error {
	if securityBackendBin == "" {
		return nftacl.Remove(tapName)
	}
	return runPlugin(securityBackendBin, "detach", PluginRequest{
		TapName: tapName, IfaceID: ifaceID, VMID: vmID, TenantID: tenantID,
	})
}

func toNftaclInterface(tap, gatewayIP, ip, mac string, p vmm.SecurityPolicy) nftacl.Interface {
	conv := func(rs []vmm.PolicyRule) []nftacl.Rule {
		var out []nftacl.Rule
		for _, r := range rs {
			out = append(out, nftacl.Rule(r))
		}
		return out
	}
	return nftacl.Interface{
		TapName: tap, GatewayIP: gatewayIP, IPAddress: ip, MACAddress: mac,
		IngressRules: conv(p.IngressRules), EgressRules: conv(p.EgressRules), Sets: toNftaclSets(p.Sets),
	}
}

func toNftaclSets(us []vmm.SetUpdate) []nftacl.SetUpdate {
	var out []nftacl.SetUpdate
	for _, u := range us {
		out = append(out, nftacl.SetUpdate{Name: u.Name, Full: u.Full, Members: u.Members, Add: u.Add, Remove: u.Remove})
	}
	return out
}

// PluginRequest is the JSON an external security-backend plugin receives on
// stdin -- exported so cmd/nftacl-snap (ServeBuiltin) decodes exactly the
// shape this package encodes. detach only ever sets TapName/IfaceID/VMID/
// TenantID; update_sets only Sets (omitempty drops the rest), same
// reasoning as netsetup's own PluginRequest: removing a port never needs
// to know what it used to be configured with.
type PluginRequest struct {
	TapName      string            `json:"tap_name,omitempty"`
	IfaceID      string            `json:"iface_id,omitempty"`
	VMID         string            `json:"vm_id,omitempty"`
	TenantID     string            `json:"tenant_id,omitempty"`
	SubnetID     string            `json:"subnet_id,omitempty"`
	SubnetLabels map[string]string `json:"subnet_labels,omitempty"`
	SubnetCIDR   string            `json:"subnet_cidr,omitempty"`
	GatewayIP    string            `json:"gateway_ip,omitempty"`
	IPAddress    string            `json:"ip_address,omitempty"`
	MACAddress   string            `json:"mac_address,omitempty"`

	// security_group_ids, ingress_rules, egress_rules and sets (attach:
	// full contents; update_sets: the changes), flattened into the
	// payload.
	vmm.SecurityPolicy

	// Network/NetworkClass context and allocated values, flattened into
	// the payload (network_id, subnet_values, ...), same as VNAP's.
	vmm.AttachInfo
}

// ServeBuiltin handles one SNAP plugin call ("attach"/"detach"/
// "update_sets" plus its payload) with the built-in nftacl backend -- the
// same thing Attach/Detach/UpdateSets do when -security-backend-bin is
// empty, reached through the plugin contract instead (versions are
// compute-agent's to check, already done by the caller). cmd/nftacl-snap
// is a thin main around this, so a SNAP shim (or anyone) can delegate
// some interfaces to stock nftacl.
func ServeBuiltin(verb string, req PluginRequest) error {
	switch verb {
	case "attach":
		if req.TapName == "" {
			return fmt.Errorf("snap: tap_name is required")
		}
		for i := range req.Sets {
			req.Sets[i].Full = true
		}
		return nftacl.Apply(toNftaclInterface(req.TapName, req.GatewayIP, req.IPAddress, req.MACAddress, req.SecurityPolicy))
	case "detach":
		if req.TapName == "" {
			return fmt.Errorf("snap: tap_name is required")
		}
		return nftacl.Remove(req.TapName)
	case "update_sets":
		return nftacl.UpdateSets(toNftaclSets(req.Sets))
	default:
		return fmt.Errorf("snap: unknown verb %q (want attach, detach or update_sets)", verb)
	}
}

// pluginTimeout mirrors netsetup's own pluginTimeout constant exactly: a
// hung security-backend plugin must not hang VM boot/teardown/ACL-update
// indefinitely.
const pluginTimeout = 10 * time.Second

// runPlugin mirrors netsetup.runPlugin's exec/stdin-JSON/exit-code contract
// verbatim -- see this package's own doc comment for why it's a separate
// contract (separate flag/binary) rather than reusing netsetup's.
func runPlugin(securityBackendBin, verb string, req PluginRequest) error {
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
