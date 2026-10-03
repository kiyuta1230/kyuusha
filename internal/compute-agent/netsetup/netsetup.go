// Package netsetup wires a VM's NetworkInterface (see docs/specs/
// network.md) to a real Linux tap device on this compute-agent's own host,
// inside its own network namespace. This is deliberately scoped to
// same-hypervisor connectivity only: every tap for a given Subnet's
// vlan_id is attached to one Linux bridge per (compute-agent process,
// vlan_id), and that bridge is given the Subnet's gateway_ip so it acts as
// a real, pingable local gateway. Two VMs on the same Subnet but different
// Hypervisors are NOT reachable from each other yet -- that needs a real
// L2 extension between hosts (a VXLAN overlay or a VLAN trunk to a
// physical uplink), which is a separate, later milestone. See
// docs/specs/network.md for what this does and doesn't cover.
//
// This built-in Linux bridge implementation is the default; an operator
// can instead delegate the local switch-attach/detach step to an external
// VNAP plugin (see Plugin's doc comment and docs/architecture.md「VMの
// ネットワーク接続をCNIのようにプラガブルにすべきか」) via -network-attach-bin.
// Wire always creates the tap device itself either way -- only the "attach
// this already-created tap to a local switch" step is pluggable.
package netsetup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Interface is everything Wire needs for one VM network attachment,
// resolved by compute (see internal/compute/network.go) from the
// NetworkInterface it created and that NetworkInterface's Subnet, and
// carried here in compute.CreateCommand (internal/compute/nats.go) plus
// the owning VM's own id/tenant (from vmm.BootSpec, not the
// NetworkInterface itself).
type Interface struct {
	IfaceID      string // the NetworkInterface's id; used to derive a deterministic tap name
	VMID         string
	TenantID     string
	SubnetID     string
	Zone         string
	SubnetLabels map[string]string
	MACAddress   string
	IPAddress    string
	SubnetCIDR   string
	GatewayIP    string // the Subnet's gateway_ip; assigned to the VLAN bridge, not the tap
	PrefixLen    int    // the Subnet CIDR's prefix length, for the bridge's gateway_ip/PrefixLen address
	VLANID       int32
	Primary      bool
}

// Wired is what Wire returns: the tap device name Firecracker's
// network-interfaces config should reference as host_dev_name, and the
// guest MAC it should be given.
type Wired struct {
	TapName    string
	MACAddress string
}

// Wire creates a persistent tap device for iface (always this package's
// own job, regardless of attachBin -- see the package doc comment), then
// attaches it to a local switch: the built-in Linux bridge implementation
// (ensures iface's VLAN bridge exists, creating it and assigning it
// GatewayIP the first time any interface for that vlan_id is wired on this
// host) when attachBin is empty, or an external VNAP plugin (see Plugin's
// doc comment) when it isn't. Idempotent either way: safe to call again
// for a tap that already exists, or a bridge another interface already
// created -- an external plugin must be idempotent too (see Plugin's doc
// comment for why).
func Wire(iface Interface, attachBin string) (*Wired, error) {
	tap := TapName(iface.IfaceID)
	if err := createPersistentTap(tap); err != nil {
		return nil, err
	}
	if attachBin == "" {
		if err := wireBuiltinBridge(iface, tap); err != nil {
			return nil, err
		}
	} else if err := runPlugin(attachBin, "attach", attachRequest(iface, tap)); err != nil {
		return nil, err
	}
	return &Wired{TapName: tap, MACAddress: iface.MACAddress}, nil
}

func wireBuiltinBridge(iface Interface, tap string) error {
	br := bridgeName(iface.VLANID)
	if err := ensureBridge(br, iface.GatewayIP, iface.PrefixLen); err != nil {
		return err
	}
	if err := runIP("link", "set", tap, "master", br); err != nil {
		return err
	}
	return runIP("link", "set", tap, "up")
}

// DeleteTap detaches tapName from whatever local switch Wire attached it
// to -- the built-in Linux bridge (where "ip link delete" below already
// does this as a side effect, so the built-in path takes no separate
// detach action) or an external VNAP plugin (attachBin non-empty) -- and
// then removes the tap device itself. ifaceID/vmID/tenantID identify which
// port to remove for the plugin's detach payload (see Plugin's doc
// comment); ignored on the built-in path. The tap is always removed
// (even if the plugin's own detach fails) so a plugin failure can never
// leak a tap device the way skipping deletion would -- see DeleteTap's
// callers in fcvmm/chvmm, which already treat this as best-effort cleanup
// and just log a non-nil error.
func DeleteTap(tapName, ifaceID, vmID, tenantID, attachBin string) error {
	var detachErr error
	if attachBin != "" {
		detachErr = runPlugin(attachBin, "detach", pluginRequest{
			TapName: tapName, IfaceID: ifaceID, VMID: vmID, TenantID: tenantID,
		})
	}
	if err := runIP("link", "delete", tapName); err != nil {
		if detachErr != nil {
			return fmt.Errorf("netsetup: detach plugin failed (%v), then delete tap also failed: %w", detachErr, err)
		}
		return err
	}
	return detachErr
}

// TapName derives a deterministic, <=15-char (IFNAMSIZ-1) Linux interface
// name from a NetworkInterface id -- ids like "netif-<16 hex chars>" are
// themselves too long for a real interface name.
func TapName(ifaceID string) string {
	sum := sha256.Sum256([]byte(ifaceID))
	return "tap" + hex.EncodeToString(sum[:6]) // "tap" + 12 hex chars = 15
}

func bridgeName(vlanID int32) string {
	return fmt.Sprintf("kbr%d", vlanID)
}

func ensureBridge(name, gatewayIP string, prefixLen int) error {
	if err := runIP("link", "add", name, "type", "bridge"); err != nil && !strings.Contains(err.Error(), "File exists") {
		return err
	}
	if err := runIP("link", "set", name, "up"); err != nil {
		return err
	}
	if gatewayIP == "" {
		return nil
	}
	// "addr replace" (not "add"): idempotent even if a previous Wire call
	// already assigned this bridge its gateway_ip.
	return runIP("addr", "replace", fmt.Sprintf("%s/%d", gatewayIP, prefixLen), "dev", name)
}

func runIP(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsetup: ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// createPersistentTap creates a tap device (IFF_TAP|IFF_NO_PI, matching
// what Firecracker itself expects to open by name) that survives past this
// process's file descriptor -- via TUNSETIFF+TUNSETPERSIST on /dev/net/tun,
// since busybox's `ip` (the only `ip` available on both this compute-agent
// image and the guest rootfs -- see docker/Dockerfile) has no `tuntap`
// subcommand, unlike the full iproute2 package.
func createPersistentTap(name string) error {
	req, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("netsetup: ifreq %s: %w", name, err)
	}
	req.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)

	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("netsetup: open /dev/net/tun: %w", err)
	}
	defer f.Close()

	if err := unix.IoctlIfreq(int(f.Fd()), unix.TUNSETIFF, req); err != nil {
		return fmt.Errorf("netsetup: TUNSETIFF %s (needs CAP_NET_ADMIN): %w", name, err)
	}
	if err := unix.IoctlSetInt(int(f.Fd()), unix.TUNSETPERSIST, 1); err != nil {
		return fmt.Errorf("netsetup: TUNSETPERSIST %s: %w", name, err)
	}
	return nil
}

// pluginRequest is the JSON a VNAP plugin (see Plugin's doc comment)
// receives on stdin. attachRequest fills every field for "attach";
// DeleteTap's detach call only ever sets TapName/IfaceID/VMID/TenantID
// (omitempty drops the rest) since removing a port never needs to know
// what it used to be configured with, only which one to remove.
type pluginRequest struct {
	TapName      string            `json:"tap_name"`
	IfaceID      string            `json:"iface_id"`
	VMID         string            `json:"vm_id"`
	TenantID     string            `json:"tenant_id"`
	SubnetID     string            `json:"subnet_id,omitempty"`
	Zone         string            `json:"zone,omitempty"`
	SubnetLabels map[string]string `json:"subnet_labels,omitempty"`
	MACAddress   string            `json:"mac_address,omitempty"`
	IPAddress    string            `json:"ip_address,omitempty"`
	SubnetCIDR   string            `json:"subnet_cidr,omitempty"`
	PrefixLen    int               `json:"prefix_len,omitempty"`
	GatewayIP    string            `json:"gateway_ip,omitempty"`
	VLANID       int32             `json:"vlan_id,omitempty"`
	Primary      bool              `json:"primary,omitempty"`
}

func attachRequest(iface Interface, tap string) pluginRequest {
	return pluginRequest{
		TapName: tap, IfaceID: iface.IfaceID, VMID: iface.VMID, TenantID: iface.TenantID,
		SubnetID: iface.SubnetID, Zone: iface.Zone, SubnetLabels: iface.SubnetLabels,
		MACAddress: iface.MACAddress, IPAddress: iface.IPAddress, SubnetCIDR: iface.SubnetCIDR, PrefixLen: iface.PrefixLen,
		GatewayIP: iface.GatewayIP, VLANID: iface.VLANID, Primary: iface.Primary,
	}
}

// pluginTimeout bounds how long an external VNAP plugin may run -- local
// networking commands (add/remove a switch port) should be near-instant; a
// hung plugin must not hang VM boot/teardown indefinitely.
const pluginTimeout = 10 * time.Second

// runPlugin is this package's side of VNAP (the "VM Network Attach
// Protocol", see docs/architecture.md「VMのネットワーク接続をCNIのように
// プラガブルにすべきか」for the full contract this implements and why it's
// deliberately NOT CNI-compatible): it execs attachBin as
// "<attachBin> <verb>" (verb is "attach" or "detach" -- never CNI's ADD/DEL,
// so nobody mistakes this for real CNI compatibility), writing req as JSON
// to its stdin.
//
// Exit code 0 is the only success signal this contract defines -- no
// structured result is expected back on stdout, unlike CNI's Result JSON:
// a VNAP plugin never creates the tap/allocates the IP (kyuusha already
// did both before ever invoking it), so it has nothing new to report.
// Non-zero exit: stderr (and stdout) are captured and folded into the
// returned error for logging. A plugin MUST be idempotent (Wire/DeleteTap
// can both be re-invoked for the same iface -- see their doc comments) and
// MUST clean up after itself before returning a non-zero exit from
// "attach" (a half-configured switch port left behind on failure is the
// plugin's own leak to avoid, not something this contract detects or
// unwinds).
func runPlugin(attachBin, verb string, req pluginRequest) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("netsetup: marshal VNAP request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, attachBin, verb)
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsetup: VNAP plugin %s %s: %w: %s", attachBin, verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}
