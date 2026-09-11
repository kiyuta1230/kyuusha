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
// it (no volume_gb here; that's block-storage's).
type tenantUsage struct {
	VCPU     int32
	MemoryMB int64
	VMCount  int32
}

// quotaChecker evaluates the used+requested<=max judgement as an OPA/Rego
// query, same foundation as internal/authz, per "Quota設計"'s explicit
// instruction to put it there rather than as hand-rolled Go comparisons.
type quotaChecker struct {
	query rego.PreparedEvalQuery
}

func newQuotaChecker(ctx context.Context) (*quotaChecker, error) {
	query, err := rego.New(
		rego.Query("data.kyuusha.compute.quota.allow"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare quota policy: %w", err)
	}
	return &quotaChecker{query: query}, nil
}

func (q *quotaChecker) allow(ctx context.Context, usage tenantUsage, requestVCPU int32, requestMemoryMB int64, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{
			"vcpu":      usage.VCPU,
			"memory_mb": usage.MemoryMB,
			"vm_count":  usage.VMCount,
		},
		"request": map[string]any{
			"vcpu":      requestVCPU,
			"memory_mb": requestMemoryMB,
		},
		"limit": map[string]any{
			"max_vcpu":             limit.GetMaxVcpu(),
			"max_memory_mb":        limit.GetMaxMemoryMb(),
			"max_vms":              limit.GetMaxVms(),
			"max_vcpu_per_vm":      limit.GetMaxVcpuPerVm(),
			"max_memory_mb_per_vm": limit.GetMaxMemoryMbPerVm(),
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
