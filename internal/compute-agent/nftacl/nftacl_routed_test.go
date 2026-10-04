package nftacl

import (
	"os"
	"os/exec"
	"testing"
)

const routedTestEnv = "NFTACL_ROUTED_TEST_IN_NETNS"

// TestTrafficRouted covers traffic the host routes rather than bridges
// (see the package doc comment's "Routed traffic"): two Subnet bridges --
// two tenants -- on one hypervisor with ip_forward=1, one VM on each. With
// no rules, neither VM may reach the other through the host, nor reach the
// host on the other tenant's gateway; each still reaches its own gateway.
// An explicit egress/ingress allow pair opens exactly that path. Runs the
// body inside a fresh network namespace (re-executing this test binary
// there) so the host's own ip_forward and nftables are never touched.
// Root only, like the rest of this package's traffic tests.
func TestTrafficRouted(t *testing.T) {
	if os.Getenv(routedTestEnv) == "" {
		if os.Geteuid() != 0 {
			t.Skip("skipping: needs root for netns/bridge/nftables")
		}
		for _, bin := range []string{"ip", "nft", "ping"} {
			if _, err := exec.LookPath(bin); err != nil {
				t.Skipf("skipping: %s not found", bin)
			}
		}
		const hv = "nftacl-routed-hv"
		_ = exec.Command("ip", "netns", "del", hv).Run()
		mustRun(t, "ip", "netns", "add", hv)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", hv).Run() })
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ip", "netns", "exec", hv, self, "-test.run", "^TestTrafficRouted$", "-test.v")
		cmd.Env = append(os.Environ(), routedTestEnv+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("in-netns run:\n%s", out)
		if err != nil {
			t.Fatalf("in-netns run failed: %v", err)
		}
		return
	}

	vms := []struct{ ns, tap, bridge, cidr, gw, ip, mac string }{
		{"nftacl-routed-a", "nftaclra", "nftaclbra", "10.123.50.0/24", "10.123.50.1", "10.123.50.10", "02:00:00:7d:00:0a"},
		{"nftacl-routed-b", "nftaclrb", "nftaclbrb", "10.123.51.0/24", "10.123.51.1", "10.123.51.10", "02:00:00:7d:00:0b"},
	}
	a, b := vms[0], vms[1]
	t.Cleanup(func() {
		for _, vm := range vms {
			_ = exec.Command("ip", "netns", "del", vm.ns).Run()
		}
	})
	mustRun(t, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	mustRun(t, "ip", "link", "set", "lo", "up")
	apply := func(vm struct{ ns, tap, bridge, cidr, gw, ip, mac string }, ingress, egress []FirewallRule) {
		t.Helper()
		if err := Apply(Interface{TapName: vm.tap, SubnetCIDR: vm.cidr, GatewayIP: vm.gw, IPAddress: vm.ip, MACAddress: vm.mac, IngressRules: ingress, EgressRules: egress}); err != nil {
			t.Fatalf("Apply %s: %v", vm.tap, err)
		}
	}
	for _, vm := range vms {
		_ = exec.Command("ip", "netns", "del", vm.ns).Run()
		mustRun(t, "ip", "netns", "add", vm.ns)
		mustRun(t, "ip", "link", "add", vm.bridge, "type", "bridge")
		mustRun(t, "ip", "addr", "add", vm.gw+"/24", "dev", vm.bridge)
		mustRun(t, "ip", "link", "set", vm.bridge, "up")
		mustRun(t, "ip", "link", "add", vm.tap, "type", "veth", "peer", "name", "eth0", "netns", vm.ns)
		mustRun(t, "ip", "link", "set", vm.tap, "master", vm.bridge, "up")
		mustRun(t, "ip", "-n", vm.ns, "link", "set", "eth0", "address", vm.mac, "up")
		mustRun(t, "ip", "-n", vm.ns, "addr", "add", vm.ip+"/24", "dev", "eth0")
		mustRun(t, "ip", "-n", vm.ns, "route", "add", "default", "via", vm.gw)
		apply(vm, nil, nil)
	}

	if err := ping(a.ns, "", a.gw); err != nil {
		t.Fatalf("A -> its own gateway failed: %v", err)
	}
	if err := ping(a.ns, "", b.ip); err == nil {
		t.Fatal("A reached B through the host with no rules allowing it")
	}
	if err := ping(a.ns, "", b.gw); err == nil {
		t.Fatal("A reached the host on B's gateway address with no rules allowing it")
	}

	// An explicit allow on both ends opens the routed path.
	apply(a, nil, []FirewallRule{{Protocol: "icmp", SourceCIDR: b.cidr, Action: "allow"}})
	apply(b, []FirewallRule{{Protocol: "icmp", SourceCIDR: a.cidr, Action: "allow"}}, nil)
	if err := ping(a.ns, "", b.ip); err != nil {
		t.Fatalf("A -> B with matching egress/ingress allows failed: %v", err)
	}

	for _, vm := range vms {
		if err := Remove(vm.tap); err != nil {
			t.Fatalf("Remove %s: %v", vm.tap, err)
		}
	}
	if out, _ := exec.Command("nft", "list", "table", "inet", table).CombinedOutput(); containsAny(string(out), a.tap, b.tap) {
		t.Fatalf("inet table still references removed taps:\n%s", out)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}
