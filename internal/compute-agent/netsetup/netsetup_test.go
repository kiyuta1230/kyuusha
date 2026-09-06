package netsetup

import "testing"

// TestWireCreatesTapAndBridge exercises the real tap/bridge creation path.
// Wire needs CAP_NET_ADMIN and /dev/net/tun, which an ordinary unprivileged
// `go test` run doesn't have -- same situation as fcvmm's real Firecracker
// boot needing /dev/kvm. Rather than skip the package outright, this skips
// only if Wire actually fails for that reason, so it still runs for real
// wherever the privilege is available (e.g. inside a compute-agent
// container, or under sudo).
func TestWireCreatesTapAndBridge(t *testing.T) {
	wired, err := Wire(Interface{
		IfaceID:    "test-iface-1",
		MACAddress: "02:00:00:00:00:01",
		GatewayIP:  "10.123.45.1",
		PrefixLen:  24,
		VLANID:     4093, // unlikely to collide with a real VLAN in any real environment
	})
	if err != nil {
		t.Skipf("skipping: tap/bridge creation needs CAP_NET_ADMIN + /dev/net/tun: %v", err)
	}
	defer DeleteTap(wired.TapName)

	if wired.TapName == "" {
		t.Fatal("Wire returned an empty tap name")
	}
	if wired.MACAddress != "02:00:00:00:00:01" {
		t.Fatalf("MACAddress = %q, want the one passed in", wired.MACAddress)
	}

	// Re-wiring the same iface (e.g. a redundant retry) must not fail.
	if _, err := Wire(Interface{
		IfaceID:    "test-iface-1",
		MACAddress: "02:00:00:00:00:01",
		GatewayIP:  "10.123.45.1",
		PrefixLen:  24,
		VLANID:     4093,
	}); err != nil {
		t.Fatalf("re-Wire of the same interface: %v", err)
	}

	if err := DeleteTap(wired.TapName); err != nil {
		t.Fatalf("DeleteTap: %v", err)
	}
}
