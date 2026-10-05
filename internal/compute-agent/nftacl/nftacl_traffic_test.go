package nftacl

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestTrafficAntiSpoof sends real frames through a real Linux bridge,
// unlike TestApplyAndRemove's `nft list ruleset` text checks: two network
// namespaces stand in for two VMs, each attached to one bridge via a veth
// whose host-side end plays the tap's role (nftacl only ever matches on
// iifname/oifname, so a veth is indistinguishable from a real tap here).
// Needs root (netns/bridge/nftables) plus ping and python3 (for crafting
// raw ARP frames) -- skipped otherwise, same skip-if-unprivileged
// convention as the rest of this package's tests. Run with e.g.
// `go test -c -o /tmp/nftacl.test ./internal/compute-agent/nftacl && sudo /tmp/nftacl.test -test.run Traffic -test.v`.
func TestTrafficAntiSpoof(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: needs root for netns/bridge/nftables")
	}
	for _, bin := range []string{"ip", "nft", "ping", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("skipping: %s not found", bin)
		}
	}

	const (
		bridge = "nftaclbr0"
		gw     = "10.123.46.1"
	)
	vms := []struct{ ns, tap, ip, mac string }{
		{"nftacl-a", "nftaclta", "10.123.46.10", "02:00:00:7e:00:0a"},
		{"nftacl-b", "nftacltb", "10.123.46.20", "02:00:00:7e:00:0b"},
	}
	a, b := vms[0], vms[1]
	memberIPs := []string{a.ip, b.ip}

	cleanup := func() {
		for _, vm := range vms {
			_ = Remove(vm.tap)
			_ = exec.Command("ip", "netns", "del", vm.ns).Run()
			_ = exec.Command("ip", "link", "del", vm.tap).Run()
		}
		_ = exec.Command("ip", "link", "del", bridge).Run()
	}
	cleanup()
	t.Cleanup(cleanup)

	mustRun(t, "ip", "link", "add", bridge, "type", "bridge")
	mustRun(t, "ip", "addr", "add", gw+"/24", "dev", bridge)
	mustRun(t, "ip", "link", "set", bridge, "up")
	for _, vm := range vms {
		mustRun(t, "ip", "netns", "add", vm.ns)
		mustRun(t, "ip", "link", "add", vm.tap, "type", "veth", "peer", "name", "eth0", "netns", vm.ns)
		mustRun(t, "ip", "link", "set", vm.tap, "master", bridge, "up")
		mustRun(t, "ip", "-n", vm.ns, "link", "set", "eth0", "address", vm.mac, "up")
		mustRun(t, "ip", "-n", vm.ns, "link", "set", "lo", "up")
		mustRun(t, "ip", "-n", vm.ns, "addr", "add", vm.ip+"/24", "dev", "eth0")
		if err := Apply(Interface{
			TapName: vm.tap, GatewayIP: gw, IPAddress: vm.ip, MACAddress: vm.mac,
			// What a Network's default SecurityGroup amounts to: in from
			// the Network's members, anything out.
			IngressRules: []Rule{{Set: "network:test"}}, EgressRules: []Rule{{CIDR: "0.0.0.0/0"}},
			Sets: []SetUpdate{{Name: "network:test", Full: true, Members: memberIPs}},
		}); err != nil {
			t.Fatalf("Apply %s: %v", vm.tap, err)
		}
	}
	// Counters inside B's own namespace: what actually arrived, independent
	// of whether any reply could make it back (a spoofed sender often can't
	// receive the reply, so "ping failed" alone proves nothing).
	mustRunStdin(t, `
table inet probe {
	chain in { type filter hook input priority 0; policy accept;
		ip saddr 10.123.46.99 counter
		ether saddr 02:00:00:7e:00:99 counter
	}
}
table arp probe {
	chain in { type filter hook input priority 0; policy accept;
		arp saddr ip 10.123.46.20 counter
	}
}
`, "ip", "netns", "exec", b.ns, "nft", "-f", "-")

	t.Run("legitimate VM-to-VM traffic passes", func(t *testing.T) {
		if err := ping(a.ns, "", b.ip); err != nil {
			t.Fatalf("A -> B ping with A's own IP/MAC failed: %v", err)
		}
	})
	t.Run("legitimate VM-to-gateway traffic passes", func(t *testing.T) {
		if err := ping(a.ns, "", gw); err != nil {
			t.Fatalf("A -> gateway ping failed: %v", err)
		}
	})

	t.Run("spoofed source IP is dropped", func(t *testing.T) {
		mustRun(t, "ip", "-n", a.ns, "addr", "add", "10.123.46.99/32", "dev", "eth0")
		defer mustRun(t, "ip", "-n", a.ns, "addr", "del", "10.123.46.99/32", "dev", "eth0")
		// Pre-seed A's neighbor entry so the probe doesn't depend on ARP at all.
		mustRun(t, "ip", "-n", a.ns, "neigh", "replace", b.ip, "lladdr", b.mac, "dev", "eth0")
		_ = ping(a.ns, "10.123.46.99", b.ip)
		if n := probeCounter(t, b.ns, "inet", "10.123.46.99"); n != 0 {
			t.Fatalf("B received %d packets from spoofed source 10.123.46.99", n)
		}
		if err := ping(a.ns, "10.123.46.99", gw); err == nil {
			t.Fatalf("gateway answered a ping from spoofed source 10.123.46.99")
		}
	})

	t.Run("spoofed source MAC is dropped", func(t *testing.T) {
		mustRun(t, "ip", "-n", a.ns, "link", "set", "eth0", "address", "02:00:00:7e:00:99")
		defer mustRun(t, "ip", "-n", a.ns, "link", "set", "eth0", "address", a.mac)
		mustRun(t, "ip", "-n", a.ns, "neigh", "replace", b.ip, "lladdr", b.mac, "dev", "eth0")
		_ = ping(a.ns, "", b.ip)
		if n := probeCounter(t, b.ns, "inet", "02:00:00:7e:00:99"); n != 0 {
			t.Fatalf("B received %d frames from spoofed MAC 02:00:00:7e:00:99", n)
		}
	})

	t.Run("spoofed ARP sender IP is dropped", func(t *testing.T) {
		// A gratuitous ARP from A (its own, real MAC) claiming B's IP.
		sendARP(t, a.ns, a.mac, b.ip)
		if n := probeCounter(t, b.ns, "arp", b.ip); n != 0 {
			t.Fatalf("B received %d ARP frames from A claiming B's IP %s", n, b.ip)
		}
	})

	t.Run("legitimate traffic still passes after spoof attempts", func(t *testing.T) {
		if err := ping(a.ns, "", b.ip); err != nil {
			t.Fatalf("A -> B ping failed: %v", err)
		}
	})
}

func ping(ns, src, dst string) error {
	args := []string{"netns", "exec", ns, "ping", "-c", "2", "-i", "0.2", "-W", "1"}
	if src != "" {
		args = append(args, "-I", src)
	}
	out, err := exec.Command("ip", append(args, dst)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// probeCounter returns the packet count of the probe rule in ns whose text
// contains match.
func probeCounter(t *testing.T, ns, family, match string) int {
	t.Helper()
	out := mustRun(t, "ip", "netns", "exec", ns, "nft", "list", "table", family, "probe")
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, match) {
			continue
		}
		f := strings.Fields(line)
		for i := range f {
			if f[i] == "packets" && i+1 < len(f) {
				n, err := strconv.Atoi(f[i+1])
				if err != nil {
					t.Fatalf("parse counter in %q: %v", line, err)
				}
				return n
			}
		}
	}
	t.Fatalf("no probe rule matching %q in:\n%s", match, out)
	return 0
}

// sendARP broadcasts a gratuitous ARP reply from ns's eth0 with the given
// sender MAC/IP, via a raw AF_PACKET socket.
func sendARP(t *testing.T, ns, senderMAC, senderIP string) {
	t.Helper()
	script := fmt.Sprintf(`
import socket
mac = bytes.fromhex(%q.replace(":", ""))
ip = socket.inet_aton(%q)
bcast = b"\xff" * 6
frame = bcast + mac + b"\x08\x06" + b"\x00\x01\x08\x00\x06\x04\x00\x02" + mac + ip + bcast + ip
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
s.bind(("eth0", 0))
for _ in range(3):
    s.send(frame)
`, senderMAC, senderIP)
	mustRun(t, "ip", "netns", "exec", ns, "python3", "-c", script)
}

func mustRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mustRunStdin(t *testing.T, stdin, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
}
