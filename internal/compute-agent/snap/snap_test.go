package snap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"
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
		Policy: vmm.SecurityPolicy{
			SecurityGroupIDs: []string{"sg-1"},
			EgressRules:      []vmm.PolicyRule{{Protocol: "tcp", PortRange: "443", CIDR: "0.0.0.0/0"}},
			IngressRules:     []vmm.PolicyRule{{Set: "sg:sg-1"}},
			Sets:             []vmm.SetUpdate{{Name: "sg:sg-1", Version: 10, Members: []string{"10.9.9.5"}}},
		},
	}, plugin)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	stdin, err := os.ReadFile(filepath.Join(dir, "stdin.json"))
	if err != nil {
		t.Fatalf("read stdin.json: %v", err)
	}
	var got PluginRequest
	if err := json.Unmarshal(stdin, &got); err != nil {
		t.Fatalf("unmarshal plugin stdin: %v", err)
	}
	want := PluginRequest{
		TapName: "tap0", IfaceID: "netif-1", VMID: "vm-1", TenantID: "tenant-1",
		SubnetID: "subnet-1", SubnetLabels: map[string]string{"vpc.example.com/id": "vpc-1"},
		SubnetCIDR: "10.9.9.0/24", GatewayIP: "10.9.9.1",
		IPAddress: "10.9.9.5", MACAddress: "02:00:00:00:00:05",
		SecurityPolicy: vmm.SecurityPolicy{
			SecurityGroupIDs: []string{"sg-1"},
			EgressRules:      []vmm.PolicyRule{{Protocol: "tcp", PortRange: "443", CIDR: "0.0.0.0/0"}},
			IngressRules:     []vmm.PolicyRule{{Set: "sg:sg-1"}},
			Sets:             []vmm.SetUpdate{{Name: "sg:sg-1", Version: 10, Full: true, Members: []string{"10.9.9.5"}}},
		},
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

// TestSetVersions: a delta must be newer than what was last applied to
// its set, a full copy at least as new; an attach's older copy is left
// out of the payload.
func TestSetVersions(t *testing.T) {
	dir := t.TempDir()
	plugin := fakePlugin(t, dir)
	sent := func() []vmm.SetUpdate {
		raw, _ := os.ReadFile(filepath.Join(dir, "stdin.json"))
		var req PluginRequest
		_ = json.Unmarshal(raw, &req)
		_ = os.Remove(filepath.Join(dir, "stdin.json"))
		return req.Sets
	}
	applied, err := UpdateSets([]vmm.SetUpdate{{Name: "sg:v", Version: 20, Add: []string{"10.0.0.1"}}}, plugin)
	if err != nil || len(applied) != 1 || len(sent()) != 1 {
		t.Fatalf("first delta: applied %v, err %v", applied, err)
	}
	for _, u := range []vmm.SetUpdate{{Name: "sg:v", Version: 20, Add: []string{"10.0.0.2"}}, {Name: "sg:v", Version: 19, Full: true}} {
		if applied, _ := UpdateSets([]vmm.SetUpdate{u}, plugin); len(applied) != 0 {
			t.Fatalf("stale update %+v was applied", u)
		}
	}
	if applied, _ := UpdateSets([]vmm.SetUpdate{{Name: "sg:v", Version: 20, Full: true}}, plugin); len(applied) != 1 {
		t.Fatal("a full copy at the same version should apply")
	}
	sent()
	if err := Attach(Interface{TapName: "tap9", Policy: vmm.SecurityPolicy{Sets: []vmm.SetUpdate{{Name: "sg:v", Version: 5}, {Name: "sg:w", Version: 5}}}}, plugin); err != nil {
		t.Fatal(err)
	}
	if got := sent(); len(got) != 1 || got[0].Name != "sg:w" {
		t.Fatalf("attach sent sets %+v, want only the fresh sg:w", got)
	}
}
