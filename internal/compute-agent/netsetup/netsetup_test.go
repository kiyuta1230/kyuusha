package netsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

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
	}, "")
	if err != nil {
		t.Skipf("skipping: tap/bridge creation needs CAP_NET_ADMIN + /dev/net/tun: %v", err)
	}
	defer DeleteTap(wired.TapName, "test-iface-1", "", "", "")

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
	}, ""); err != nil {
		t.Fatalf("re-Wire of the same interface: %v", err)
	}

	if err := DeleteTap(wired.TapName, "test-iface-1", "", "", ""); err != nil {
		t.Fatalf("DeleteTap: %v", err)
	}
}

// fakeVNAPPlugin writes a shell script standing in for an external VNAP
// plugin (see runPlugin's doc comment): it records argv[1] (the verb) and
// its stdin payload to files under dir, for the test to inspect, then
// exits with exitCode.
func fakeVNAPPlugin(t *testing.T, dir string, exitCode int) string {
	t.Helper()
	script := filepath.Join(dir, "vnap-plugin.sh")
	body := "#!/bin/sh\n" +
		"echo \"$1\" > \"" + dir + "/verb\"\n" +
		"cat > \"" + dir + "/stdin.json\"\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake plugin: %v", err)
	}
	return script
}

func TestWireInvokesExternalPluginWithFullAttachPayload(t *testing.T) {
	dir := t.TempDir()
	plugin := fakeVNAPPlugin(t, dir, 0)

	wired, err := Wire(Interface{
		IfaceID: "test-iface-plugin", VMID: "vm-1", TenantID: "tenant-1",
		MACAddress: "02:00:00:00:00:02", IPAddress: "10.9.9.5", GatewayIP: "10.9.9.1",
		PrefixLen: 24, VLANID: 42, Primary: true,
	}, plugin)
	if err != nil {
		t.Skipf("skipping: tap creation needs CAP_NET_ADMIN + /dev/net/tun: %v", err)
	}
	defer DeleteTap(wired.TapName, "test-iface-plugin", "vm-1", "tenant-1", plugin)

	verb, err := os.ReadFile(filepath.Join(dir, "verb"))
	if err != nil {
		t.Fatalf("read verb: %v", err)
	}
	if string(verb) != "attach\n" {
		t.Fatalf("verb = %q, want \"attach\\n\"", verb)
	}

	var got pluginRequest
	stdin, err := os.ReadFile(filepath.Join(dir, "stdin.json"))
	if err != nil {
		t.Fatalf("read stdin.json: %v", err)
	}
	if err := json.Unmarshal(stdin, &got); err != nil {
		t.Fatalf("unmarshal plugin stdin: %v", err)
	}
	want := pluginRequest{
		TapName: wired.TapName, IfaceID: "test-iface-plugin", VMID: "vm-1", TenantID: "tenant-1",
		MACAddress: "02:00:00:00:00:02", IPAddress: "10.9.9.5", GatewayIP: "10.9.9.1",
		PrefixLen: 24, VLANID: 42, Primary: true,
	}
	if got != want {
		t.Fatalf("plugin attach payload = %+v, want %+v", got, want)
	}
}

func TestDeleteTapDetachPayloadOmitsAttachOnlyFields(t *testing.T) {
	dir := t.TempDir()
	plugin := fakeVNAPPlugin(t, dir, 0)

	wired, err := Wire(Interface{IfaceID: "test-iface-detach", VLANID: 42}, plugin)
	if err != nil {
		t.Skipf("skipping: tap creation needs CAP_NET_ADMIN + /dev/net/tun: %v", err)
	}

	if err := DeleteTap(wired.TapName, "test-iface-detach", "vm-2", "tenant-2", plugin); err != nil {
		t.Fatalf("DeleteTap: %v", err)
	}

	verb, err := os.ReadFile(filepath.Join(dir, "verb"))
	if err != nil {
		t.Fatalf("read verb: %v", err)
	}
	if string(verb) != "detach\n" {
		t.Fatalf("verb = %q, want \"detach\\n\"", verb)
	}
	stdin, err := os.ReadFile(filepath.Join(dir, "stdin.json"))
	if err != nil {
		t.Fatalf("read stdin.json: %v", err)
	}
	// Only identifying fields -- no mac/ip/gateway/vlan, since detach never
	// needs to know what the port used to be configured with.
	wantJSON := `{"tap_name":"` + wired.TapName + `","iface_id":"test-iface-detach","vm_id":"vm-2","tenant_id":"tenant-2"}`
	if string(stdin) != wantJSON {
		t.Fatalf("detach payload = %s, want %s", stdin, wantJSON)
	}
}

func TestDeleteTapStillDeletesTapWhenPluginDetachFails(t *testing.T) {
	dir := t.TempDir()
	failingPlugin := fakeVNAPPlugin(t, dir, 1)

	wired, err := Wire(Interface{IfaceID: "test-iface-fail-detach", VLANID: 42}, "")
	if err != nil {
		t.Skipf("skipping: tap creation needs CAP_NET_ADMIN + /dev/net/tun: %v", err)
	}

	// The plugin's detach fails, but the tap must still be gone afterward --
	// see DeleteTap's doc comment on why a plugin failure must never leak
	// the tap device.
	err = DeleteTap(wired.TapName, "test-iface-fail-detach", "", "", failingPlugin)
	if err == nil {
		t.Fatal("DeleteTap with a failing plugin: got nil error, want non-nil")
	}
	if _, statErr := os.Stat("/sys/class/net/" + wired.TapName); !os.IsNotExist(statErr) {
		t.Fatalf("tap device %s still exists after DeleteTap despite plugin failure", wired.TapName)
	}
}
