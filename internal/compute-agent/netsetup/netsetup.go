// Package netsetup wires a Firecracker VM's NetworkInterface (see
// docs/specs/network.md) to a real Linux tap device on this compute-agent's
// own host, inside its own network namespace. This is deliberately scoped
// to same-hypervisor connectivity only: every tap for a given Subnet's
// vlan_id is attached to one Linux bridge per (compute-agent process,
// vlan_id), and that bridge is given the Subnet's gateway_ip so it acts as
// a real, pingable local gateway. Two VMs on the same Subnet but different
// Hypervisors are NOT reachable from each other yet -- that needs a real
// L2 extension between hosts (a VXLAN overlay or a VLAN trunk to a
// physical uplink), which is a separate, later milestone. See
// docs/specs/network.md for what this does and doesn't cover.
package netsetup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// Interface is everything Wire needs for one VM network attachment,
// resolved by compute (see internal/compute/network.go) from the
// NetworkInterface it created and that NetworkInterface's Subnet, and
// carried here in compute.CreateCommand (internal/compute/nats.go).
type Interface struct {
	IfaceID    string // the NetworkInterface's id; used to derive a deterministic tap name
	MACAddress string
	GatewayIP  string // the Subnet's gateway_ip; assigned to the VLAN bridge, not the tap
	PrefixLen  int    // the Subnet CIDR's prefix length, for the bridge's gateway_ip/PrefixLen address
	VLANID     int32
}

// Wired is what Wire returns: the tap device name Firecracker's
// network-interfaces config should reference as host_dev_name, and the
// guest MAC it should be given.
type Wired struct {
	TapName    string
	MACAddress string
}

// Wire ensures iface's VLAN bridge exists (creating it and assigning it
// GatewayIP the first time any interface for that vlan_id is wired on this
// host), creates a persistent tap device for iface, and attaches it to
// that bridge. Idempotent: safe to call again for a tap that already
// exists, or a bridge another interface already created.
func Wire(iface Interface) (*Wired, error) {
	br := bridgeName(iface.VLANID)
	if err := ensureBridge(br, iface.GatewayIP, iface.PrefixLen); err != nil {
		return nil, err
	}
	tap := TapName(iface.IfaceID)
	if err := createPersistentTap(tap); err != nil {
		return nil, err
	}
	if err := runIP("link", "set", tap, "master", br); err != nil {
		return nil, err
	}
	if err := runIP("link", "set", tap, "up"); err != nil {
		return nil, err
	}
	return &Wired{TapName: tap, MACAddress: iface.MACAddress}, nil
}

// DeleteTap removes a tap device created by Wire (by its already-derived
// name -- see TapName). The VLAN bridge itself is left in place: it's
// shared by every VM on this Hypervisor for that Subnet, and only ever
// torn down along with the whole compute-agent process (which drops its
// network namespace entirely, taking the bridge with it).
func DeleteTap(tapName string) error {
	return runIP("link", "delete", tapName)
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
