package blockstorage

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

//go:embed quota.rego
var quotaPolicySrc string

// tenantUsage mirrors docs/architecture.md's tenant_usage(tenant_id, ...,
// used_volume_gb, ...) -- block-storage's own local half of it (no vcpu/
// memory_mb/vm_count here; that's compute's, see internal/compute/quota.go).
type tenantUsage struct {
	VolumeGB int64
}

// quotaChecker evaluates the used+requested<=max judgement as an OPA/Rego
// query, same foundation as internal/authz and compute's own quotaChecker,
// per "Quota設計"'s explicit instruction to put it there rather than as
// hand-rolled Go comparisons.
type quotaChecker struct {
	query rego.PreparedEvalQuery
}

func newQuotaChecker(ctx context.Context) (*quotaChecker, error) {
	query, err := rego.New(
		rego.Query("data.kyuusha.blockstorage.quota.allow"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare quota policy: %w", err)
	}
	return &quotaChecker{query: query}, nil
}

func (q *quotaChecker) allow(ctx context.Context, usage tenantUsage, requestSizeGB int64, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{
			"volume_gb": usage.VolumeGB,
		},
		"request": map[string]any{
			"size_gb": requestSizeGB,
		},
		"limit": map[string]any{
			"max_volume_gb": limit.GetMaxVolumeGb(),
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
// means tenantID doesn't name a real Tenant, surfaced as ErrValidation like
// any other Create-time input problem -- same as compute's identical
// helper.
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
