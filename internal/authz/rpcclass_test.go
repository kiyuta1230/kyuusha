package authz

import "testing"

func TestRPCService(t *testing.T) {
	tests := map[string]string{
		"/kyuusha.blockstorage.v1.StorageConnectionService/Create": "blockstorage",
		"/kyuusha.blockstorage.v1.VolumeService/Get":               "blockstorage",
		"/kyuusha.compute.v1.VirtualMachineService/Create":         "compute",
		"/kyuusha.compute.v1.HypervisorService/Register":           "compute",
		"/kyuusha.identity.v1.TenantService/Create":                "identity",
		"/kyuusha.image.v1.ImageService/List":                      "image",
		"/kyuusha.network.v1.SubnetService/Watch":                  "network",
		"not-even-a-method":                                        "",
	}
	for method, want := range tests {
		if got := rpcService(method); got != want {
			t.Errorf("rpcService(%q) = %q, want %q", method, got, want)
		}
	}
}

func TestRPCAction(t *testing.T) {
	tests := map[string]string{
		"/kyuusha.compute.v1.VirtualMachineService/Get":           "read",
		"/kyuusha.compute.v1.VirtualMachineService/List":          "read",
		"/kyuusha.compute.v1.VirtualMachineService/Watch":         "read",
		"/kyuusha.compute.v1.VirtualMachineService/Create":        "write",
		"/kyuusha.compute.v1.VirtualMachineService/Update":        "write",
		"/kyuusha.compute.v1.VirtualMachineService/Delete":        "write",
		"/kyuusha.compute.v1.VirtualMachineService/StreamConsole": "write",
		"/kyuusha.compute.v1.HypervisorService/Register":          "write",
		"/kyuusha.compute.v1.HypervisorService/SetSchedulable":    "write",
		"/kyuusha.image.v1.ImageService/SetVisibility":            "write",
		"not-even-a-method": "write",
	}
	for method, want := range tests {
		if got := rpcAction(method); got != want {
			t.Errorf("rpcAction(%q) = %q, want %q", method, got, want)
		}
	}
}
