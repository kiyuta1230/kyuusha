package compute

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

//go:embed quota.rego
var quotaPolicySrc string

// tenantUsage mirrors docs/architecture.md's tenant_usage(tenant_id,
// used_vcpu, used_memory_mb, ..., vm_count) -- compute's own local half of
// it (no volume_gb here; that's block-storage's). PciDevices is keyed by
// (vendor_id, device_id) rather than a flat count: quota is enforced per
// device type (see quota.rego's pci_usage_count), not in aggregate.
type tenantUsage struct {
	VCPU       int32
	MemoryMB   int64
	VMCount    int32
	PciDevices map[pciDeviceKey]int32
}

// quotaChecker evaluates the used+requested<=max judgement as an OPA/Rego
// query, same foundation as internal/authz, per "Quota設計"'s explicit
// instruction to put it there rather than as hand-rolled Go comparisons.
type quotaChecker struct {
	query       rego.PreparedEvalQuery
	resizeQuery rego.PreparedEvalQuery
}

func newQuotaChecker(ctx context.Context) (*quotaChecker, error) {
	query, err := rego.New(
		rego.Query("data.kyuusha.compute.quota.allow"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare quota policy: %w", err)
	}
	resizeQuery, err := rego.New(
		rego.Query("data.kyuusha.compute.quota.allow_resize"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare quota resize policy: %w", err)
	}
	return &quotaChecker{query: query, resizeQuery: resizeQuery}, nil
}

func (q *quotaChecker) allow(ctx context.Context, usage tenantUsage, requestVCPU int32, requestMemoryMB int64, requestPciDevices []PciDeviceRequest, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{
			"vcpu":        usage.VCPU,
			"memory_mb":   usage.MemoryMB,
			"vm_count":    usage.VMCount,
			"pci_devices": pciUsageInput(usage.PciDevices),
		},
		"request": map[string]any{
			"vcpu":        requestVCPU,
			"memory_mb":   requestMemoryMB,
			"pci_devices": pciRequestInput(requestPciDevices),
		},
		"limit": map[string]any{
			"max_vcpu":             limit.GetMaxVcpu(),
			"max_memory_mb":        limit.GetMaxMemoryMb(),
			"max_vms":              limit.GetMaxVms(),
			"max_vcpu_per_vm":      limit.GetMaxVcpuPerVm(),
			"max_memory_mb_per_vm": limit.GetMaxMemoryMbPerVm(),
			"pci_devices":          pciLimitInput(limit.GetPciDevices()),
		},
	}
	results, err := q.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return false, fmt.Errorf("evaluate quota policy: %w", err)
	}
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return false, nil
	}
	allowed, _ := results[0].Expressions[0].Value.(bool)
	return allowed, nil
}

// addPciUsage charges (sign=1) or refunds (sign=-1) requested PCI device
// counts against a tenantUsage, lazily allocating the map -- mirroring the
// plain VCPU/MemoryMB +=/-= lines beside every call site, just keyed.
func addPciUsage(u *tenantUsage, devices []PciDeviceRequest, sign int32) {
	if len(devices) == 0 {
		return
	}
	if u.PciDevices == nil {
		u.PciDevices = make(map[pciDeviceKey]int32, len(devices))
	}
	for _, d := range devices {
		key := pciDeviceKey{vendorID: d.VendorID, deviceID: d.DeviceID}
		u.PciDevices[key] += sign * pciCount(d)
	}
}

func pciRequestInput(reqs []PciDeviceRequest) []map[string]any {
	out := make([]map[string]any, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, map[string]any{"vendor_id": r.VendorID, "device_id": r.DeviceID, "count": pciCount(r)})
	}
	return out
}

func pciUsageInput(usage map[pciDeviceKey]int32) []map[string]any {
	out := make([]map[string]any, 0, len(usage))
	for k, count := range usage {
		out = append(out, map[string]any{"vendor_id": k.vendorID, "device_id": k.deviceID, "count": count})
	}
	return out
}

func pciLimitInput(limits []*identityv1.PciDeviceQuota) []map[string]any {
	out := make([]map[string]any, 0, len(limits))
	for _, l := range limits {
		out = append(out, map[string]any{"vendor_id": l.GetVendorId(), "device_id": l.GetDeviceId(), "max_count": l.GetMaxCount()})
	}
	return out
}

// allowResize is Resize's counterpart to allow: see quota.rego's
// allow_resize for why this is a separate rule rather than reusing allow
// (vm_count doesn't change, and the aggregate check is against a delta, not
// an absolute new usage).
func (q *quotaChecker) allowResize(ctx context.Context, usage tenantUsage, deltaVCPU int32, deltaMemoryMB int64, newVCPU int32, newMemoryMB int64, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{
			"vcpu":      usage.VCPU,
			"memory_mb": usage.MemoryMB,
			"vm_count":  usage.VMCount,
		},
		"request": map[string]any{
			"delta_vcpu":      deltaVCPU,
			"delta_memory_mb": deltaMemoryMB,
			"new_vcpu":        newVCPU,
			"new_memory_mb":   newMemoryMB,
		},
		"limit": map[string]any{
			"max_vcpu":             limit.GetMaxVcpu(),
			"max_memory_mb":        limit.GetMaxMemoryMb(),
			"max_vms":              limit.GetMaxVms(),
			"max_vcpu_per_vm":      limit.GetMaxVcpuPerVm(),
			"max_memory_mb_per_vm": limit.GetMaxMemoryMbPerVm(),
		},
	}
	results, err := q.resizeQuery.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return false, fmt.Errorf("evaluate quota resize policy: %w", err)
	}
	if len(results) == 0 || len(results[0].Expressions) == 0 {
		return false, nil
	}
	allowed, _ := results[0].Expressions[0].Value.(bool)
	return allowed, nil
}

// lookupQuota fetches tenantID's QuotaSpec from identity. A NotFound there
// means tenantID doesn't name a real Tenant, which is now something Create
// can actually detect (it couldn't before identity existed) -- surfaced as
// ErrValidation like any other Create-time input problem. Any other error
// (identity unreachable, etc.) is returned as-is: it's already a transport-
// level status error from the identity client call, not a domain error, so
// grpcserver.toStatus's fallback passes it straight through unchanged.
func lookupQuota(ctx context.Context, client identityv1.TenantServiceClient, tenantID string) (*identityv1.QuotaSpec, error) {
	tn, err := client.Get(ctx, &identityv1.GetTenantRequest{TenantId: tenantID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("%w: tenant_id %q does not exist", ErrValidation, tenantID)
		}
		return nil, err
	}
	return tn.GetSpec().GetQuota(), nil
}
