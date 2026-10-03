package snap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakePlugin writes a shell script that records its verb and stdin into
// dir, then exits 0 -- the same fake-plugin approach netsetup's own tests
// use for VNAP.
func fakePlugin(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "plugin.sh")
	body := "#!/bin/sh\necho \"$1\" > " + filepath.Join(dir, "verb") + "\ncat > " + filepath.Join(dir, "stdin.json") + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake plugin: %v", err)
	}
	return script
}

func TestAttachSendsAddressAndSubnetToPlugin(t *testing.T) {
	dir := t.TempDir()
	plugin := fakePlugin(t, dir)

	err := Attach(Interface{
		IfaceID: "netif-1", VMID: "vm-1", TenantID: "tenant-1", TapName: "tap0",
		SubnetID: "subnet-1", SubnetLabels: map[string]string{"vpc.example.com/id": "vpc-1"},
		SubnetCIDR: "10.9.9.0/24", GatewayIP: "10.9.9.1",
		IPAddress: "10.9.9.5", MACAddress: "02:00:00:00:00:05",
		EgressRules: []FirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "0.0.0.0/0", Action: "allow"}},
	}, plugin)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	stdin, err := os.ReadFile(filepath.Join(dir, "stdin.json"))
	if err != nil {
		t.Fatalf("read stdin.json: %v", err)
	}
	var got pluginRequest
	if err := json.Unmarshal(stdin, &got); err != nil {
		t.Fatalf("unmarshal plugin stdin: %v", err)
	}
	want := pluginRequest{
		TapName: "tap0", IfaceID: "netif-1", VMID: "vm-1", TenantID: "tenant-1",
		SubnetID: "subnet-1", SubnetLabels: map[string]string{"vpc.example.com/id": "vpc-1"},
		SubnetCIDR: "10.9.9.0/24", GatewayIP: "10.9.9.1",
		IPAddress: "10.9.9.5", MACAddress: "02:00:00:00:00:05",
		EgressRules: []pluginFirewallRule{{Protocol: "tcp", PortRange: "443", SourceCIDR: "0.0.0.0/0", Action: "allow"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attach payload = %+v, want %+v", got, want)
	}
}

func TestDetachPayloadCarriesIdentifiersOnly(t *testing.T) {
	dir := t.TempDir()
	plugin := fakePlugin(t, dir)

	if err := Detach("netif-1", "vm-1", "tenant-1", "tap0", plugin); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	stdin, err := os.ReadFile(filepath.Join(dir, "stdin.json"))
	if err != nil {
		t.Fatalf("read stdin.json: %v", err)
	}
	want := `{"tap_name":"tap0","iface_id":"netif-1","vm_id":"vm-1","tenant_id":"tenant-1"}`
	if string(stdin) != want {
		t.Fatalf("detach payload = %s, want %s", stdin, want)
	}
}
