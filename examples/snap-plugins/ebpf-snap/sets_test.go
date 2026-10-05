package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cilium/ebpf"
)

func clearConntrack(t *testing.T) {
	t.Helper()
	m, err := ebpf.LoadPinnedMap(filepath.Join(pinRoot, "conntrack"), nil)
	if err != nil {
		t.Fatalf("load conntrack: %v", err)
	}
	defer m.Close()
	var k bpfConntrackKey
	var v uint64
	var keys []bpfConntrackKey
	it := m.Iterate()
	for it.Next(&k, &v) {
		keys = append(keys, k)
	}
	for _, k := range keys {
		_ = m.Delete(k)
	}
}

// TestAddressSets checks a set-referencing ingress rule against real
// traffic: a host address on the tap's side may reach the VM only while
// it's a member of the set, as update_sets adds and removes it. Needs
// root, like TestAntiSpoof.
func TestAddressSets(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: needs root for netns/TCX/bpffs")
	}
	const (
		ns    = "ebpfsnap-sets"
		tap   = "ebpfsnaptap1"
		gw    = "10.123.48.1"
		peer  = "10.123.48.2" // a second host-side address: not the gateway, so only the set can allow it
		vmIP  = "10.123.48.10"
		vmMAC = "02:00:00:7f:01:0a"
		set   = "sg:sets-test"
	)
	cleanup := func() {
		_ = detach(pluginRequest{TapName: tap})
		_ = exec.Command("ip", "netns", "del", ns).Run()
		_ = exec.Command("ip", "link", "del", tap).Run()
	}
	cleanup()
	t.Cleanup(cleanup)
	mustRun(t, "ip", "netns", "add", ns)
	mustRun(t, "ip", "link", "add", tap, "type", "veth", "peer", "name", "eth0", "netns", ns)
	mustRun(t, "ip", "addr", "add", gw+"/24", "dev", tap)
	mustRun(t, "ip", "addr", "add", peer+"/24", "dev", tap)
	mustRun(t, "ip", "link", "set", tap, "up")
	mustRun(t, "ip", "-n", ns, "link", "set", "eth0", "address", vmMAC, "up")
	mustRun(t, "ip", "-n", ns, "addr", "add", vmIP+"/24", "dev", "eth0")

	if err := attach(pluginRequest{
		TapName: tap, GatewayIP: gw, IPAddress: vmIP, MACAddress: vmMAC,
		IngressRules: []policyRule{{Protocol: "icmp", Set: set}},
		Sets:         []setUpdate{{Name: set, Version: 1, Members: []string{"10.123.48.99"}}},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	clearConntrack(t) // a previous run's flow may still be live
	hostPing := func() error {
		return exec.Command("ping", "-c", "2", "-i", "0.2", "-W", "1", "-I", peer, vmIP).Run()
	}
	if hostPing() == nil {
		t.Fatal("non-member reached the VM")
	}
	if err := updateSets([]setUpdate{{Name: set, Version: 2, Add: []string{peer}}}); err != nil {
		t.Fatal(err)
	}
	if err := hostPing(); err != nil {
		t.Fatalf("member could not reach the VM: %v", err)
	}
	if err := updateSets([]setUpdate{{Name: set, Version: 3, Remove: []string{peer}}}); err != nil {
		t.Fatal(err)
	}
	// An established flow outlives the removal (stateful, like any
	// SecurityGroup), and this plugin's conntrack keys ICMP without its id:
	// forget the earlier pings' flow so the next one is judged afresh.
	clearConntrack(t)
	if hostPing() == nil {
		t.Fatal("removed member still reached the VM")
	}
	if err := updateSets([]setUpdate{{Name: set, Version: 4, Full: true, Members: []string{peer}}}); err != nil {
		t.Fatal(err)
	}
	if err := hostPing(); err != nil {
		t.Fatalf("member from a full copy could not reach the VM: %v", err)
	}
}
