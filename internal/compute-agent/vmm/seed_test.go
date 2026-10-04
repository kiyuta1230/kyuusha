package vmm

import (
	"strings"
	"testing"
)

func TestBuildNetworkConfig(t *testing.T) {
	got := buildNetworkConfig([]NetIface{
		{IPAddress: "10.0.1.5", PrefixLen: 24, GatewayIP: "10.0.1.1", Primary: true,
			DNSServers: []string{"10.0.0.53", "10.0.0.54"}, DNSSearch: "cluster.example", Attach: AttachInfo{MTU: 1450}},
		{IPAddress: "10.0.2.5", PrefixLen: 24, GatewayIP: "10.0.2.1", DNSServers: []string{"10.9.9.9"}},
		{}, // no address yet: skipped
	})
	for _, want := range []string{
		"  eth0:\n    addresses: [10.0.1.5/24]\n    gateway4: 10.0.1.1\n    mtu: 1450\n    nameservers:\n      addresses: [10.0.0.53, 10.0.0.54]\n      search: [cluster.example]\n",
		"  eth1:\n    addresses: [10.0.2.5/24]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("network-config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "10.9.9.9") || strings.Contains(got, "eth2") {
		t.Errorf("resolvers only go on the primary interface, and address-less NICs are skipped:\n%s", got)
	}
	if buildNetworkConfig(nil) != "" {
		t.Error("no interfaces should produce no network-config")
	}
}
