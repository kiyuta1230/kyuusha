package image

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

// tenantUsage mirrors docs/architecture.md's tenant_usage(tenant_id, ...) --
// image's own local half of it (just a count, no size/memory/vcpu
// dimension; see internal/compute/quota.go and internal/block-storage/
// quota.go for those services' own local halves).
type tenantUsage struct {
	ImageCount int32
}

// quotaChecker evaluates the used+requested<=max judgement as an OPA/Rego
// query, same foundation as internal/authz and compute/block-storage's own
// quotaCheckers, per "Quota設計"'s explicit instruction to put it there
// rather than as hand-rolled Go comparisons.
type quotaChecker struct {
	query rego.PreparedEvalQuery
}

func newQuotaChecker(ctx context.Context) (*quotaChecker, error) {
	query, err := rego.New(
		rego.Query("data.kyuusha.image.quota.allow"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare quota policy: %w", err)
	}
	return &quotaChecker{query: query}, nil
}

func (q *quotaChecker) allow(ctx context.Context, usage tenantUsage, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{
			"image_count": usage.ImageCount,
		},
		"limit": map[string]any{
			"max_images": limit.GetMaxImages(),
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
// any other Create-time input problem -- same as compute/block-storage's
// identical helper.
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
