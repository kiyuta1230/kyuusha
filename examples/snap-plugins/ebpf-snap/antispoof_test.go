package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestAntiSpoof sends real frames from a network namespace (standing in
// for the VM) over a veth pair whose root-namespace end plays the tap's
// role, with this plugin's attach() applied to it. Counters in the root
// namespace's nftables input hook record what actually got past the TC
// ingress program (TCX runs before netfilter). Needs root, a TCX-capable
// kernel, /sys/fs/bpf, ping and python3 -- skipped when not root. Run with
// `go test -c -o /tmp/ebpf-snap.test . && sudo /tmp/ebpf-snap.test -test.v`.
func TestAntiSpoof(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("skipping: needs root for netns/TCX/bpffs")
	}
	for _, bin := range []string{"ip", "nft", "ping", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("skipping: %s not found", bin)
		}
	}

	const (
		ns     = "ebpfsnap-vm"
		tap    = "ebpfsnaptap0"
		gw     = "10.123.47.1"
		vmIP   = "10.123.47.10"
		vmMAC  = "02:00:00:7f:00:0a"
		fakeIP = "10.123.47.99"
		fakeMC = "02:00:00:7f:00:99"
	)
	cleanup := func() {
		_ = detach(pluginRequest{TapName: tap})
		_ = exec.Command("ip", "netns", "del", ns).Run()
		_ = exec.Command("ip", "link", "del", tap).Run()
		_ = exec.Command("nft", "delete", "table", "inet", "ebpfsnapprobe").Run()
		_ = exec.Command("nft", "delete", "table", "arp", "ebpfsnapprobe").Run()
	}
	cleanup()
	t.Cleanup(cleanup)

	mustRun(t, "ip", "netns", "add", ns)
	mustRun(t, "ip", "link", "add", tap, "type", "veth", "peer", "name", "eth0", "netns", ns)
	mustRun(t, "ip", "addr", "add", gw+"/24", "dev", tap)
	mustRun(t, "ip", "link", "set", tap, "up")
	mustRun(t, "ip", "-n", ns, "link", "set", "eth0", "address", vmMAC, "up")
	mustRun(t, "ip", "-n", ns, "addr", "add", vmIP+"/24", "dev", "eth0")

	if err := attach(pluginRequest{
		TapName: tap, SubnetCIDR: "10.123.47.0/24", GatewayIP: gw,
		IPAddress: vmIP, MACAddress: vmMAC,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	mustRunStdin(t, fmt.Sprintf(`
table inet ebpfsnapprobe {
	chain in { type filter hook input priority 0; policy accept;
		iifname %q ip saddr %s counter
		iifname %q ether saddr %s counter
	}
}
table arp ebpfsnapprobe {
	chain in { type filter hook input priority 0; policy accept;
		arp saddr ip 10.123.47.20 counter
	}
}
`, tap, fakeIP, tap, fakeMC), "nft", "-f", "-")

	t.Run("legitimate traffic passes", func(t *testing.T) {
		if err := ping(ns, "", gw); err != nil {
			t.Fatalf("VM -> gateway ping failed: %v", err)
		}
	})
	t.Run("spoofed source IP is dropped", func(t *testing.T) {
		mustRun(t, "ip", "-n", ns, "addr", "add", fakeIP+"/32", "dev", "eth0")
		defer mustRun(t, "ip", "-n", ns, "addr", "del", fakeIP+"/32", "dev", "eth0")
		_ = ping(ns, fakeIP, gw)
		if n := counter(t, "inet", fakeIP); n != 0 {
			t.Fatalf("%d packets from spoofed source %s got past TC ingress", n, fakeIP)
		}
	})
	t.Run("spoofed source MAC is dropped", func(t *testing.T) {
		mustRun(t, "ip", "-n", ns, "link", "set", "eth0", "address", fakeMC)
		defer mustRun(t, "ip", "-n", ns, "link", "set", "eth0", "address", vmMAC)
		_ = ping(ns, "", gw)
		if n := counter(t, "inet", fakeMC); n != 0 {
			t.Fatalf("%d frames from spoofed MAC %s got past TC ingress", n, fakeMC)
		}
	})
	t.Run("spoofed ARP sender IP is dropped", func(t *testing.T) {
		sendARP(t, ns, vmMAC, "10.123.47.20")
		if n := counter(t, "arp", "10.123.47.20"); n != 0 {
			t.Fatalf("%d ARP frames claiming 10.123.47.20 got past TC ingress", n)
		}
	})
	t.Run("re-attach without an address keeps the check", func(t *testing.T) {
		if err := attach(pluginRequest{TapName: tap, SubnetCIDR: "10.123.47.0/24", GatewayIP: gw}); err != nil {
			t.Fatalf("re-attach: %v", err)
		}
		mustRun(t, "ip", "-n", ns, "addr", "add", fakeIP+"/32", "dev", "eth0")
		defer mustRun(t, "ip", "-n", ns, "addr", "del", fakeIP+"/32", "dev", "eth0")
		_ = ping(ns, fakeIP, gw)
		if n := counter(t, "inet", fakeIP); n != 0 {
			t.Fatalf("%d packets from spoofed source %s got past TC ingress after re-attach", n, fakeIP)
		}
	})
	t.Run("legitimate traffic still passes", func(t *testing.T) {
		if err := ping(ns, "", gw); err != nil {
			t.Fatalf("VM -> gateway ping failed: %v", err)
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

func counter(t *testing.T, family, match string) int {
	t.Helper()
	out := mustRun(t, "nft", "list", "table", family, "ebpfsnapprobe")
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
