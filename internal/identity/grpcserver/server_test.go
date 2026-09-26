package grpcserver

import (
	"testing"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

// TestQuotaRoundTrip guards against the class of bug found 2026-09-27: a
// new QuotaSpec field (MaxImages/MaxSubnets/MaxNetworkInterfaces) was added
// to the proto and to every quota-enforcing service, but fromQuota/toQuota
// here were never updated to carry it, so it silently dropped on every
// Tenant Create/Update/Get round trip. Every QuotaSpec field must appear
// here so a future field gets the same protection.
func TestQuotaRoundTrip(t *testing.T) {
	in := &identityv1.QuotaSpec{
		MaxVcpu:              1,
		MaxMemoryMb:          2,
		MaxVolumeGb:          3,
		MaxVms:               4,
		MaxVcpuPerVm:         5,
		MaxMemoryMbPerVm:     6,
		MaxImages:            7,
		MaxSubnets:           8,
		MaxNetworkInterfaces: 9,
		PciDevices: []*identityv1.PciDeviceQuota{
			{VendorId: "10de", DeviceId: "1c03", MaxCount: 2},
		},
	}

	out := toQuota(fromQuota(in))

	if out.GetMaxVcpu() != in.GetMaxVcpu() ||
		out.GetMaxMemoryMb() != in.GetMaxMemoryMb() ||
		out.GetMaxVolumeGb() != in.GetMaxVolumeGb() ||
		out.GetMaxVms() != in.GetMaxVms() ||
		out.GetMaxVcpuPerVm() != in.GetMaxVcpuPerVm() ||
		out.GetMaxMemoryMbPerVm() != in.GetMaxMemoryMbPerVm() ||
		out.GetMaxImages() != in.GetMaxImages() ||
		out.GetMaxSubnets() != in.GetMaxSubnets() ||
		out.GetMaxNetworkInterfaces() != in.GetMaxNetworkInterfaces() {
		t.Fatalf("round trip mismatch: in=%+v out=%+v", in, out)
	}
	if len(out.GetPciDevices()) != 1 ||
		out.GetPciDevices()[0].GetVendorId() != "10de" ||
		out.GetPciDevices()[0].GetDeviceId() != "1c03" ||
		out.GetPciDevices()[0].GetMaxCount() != 2 {
		t.Fatalf("PciDevices round trip mismatch: got %+v", out.GetPciDevices())
	}
}
