package network

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
// network's own local half of it (two counts, no size/memory/vcpu
// dimension; see internal/compute/quota.go and internal/block-storage/
// quota.go for those services' own local halves).
type tenantUsage struct {
	SubnetCount           int32
	NetworkInterfaceCount int32
	IPReservationCount    int32
}

// quotaChecker evaluates the used+requested<=max judgement as an OPA/Rego
// query, same foundation as internal/authz and compute/block-storage/
// image's own quotaCheckers, per "Quota設計"'s explicit instruction to put
// it there rather than as hand-rolled Go comparisons. Two queries (not one),
// mirroring compute's query/resizeQuery split -- CreateSubnet and
// CreateNetworkInterface each need their own rego rule (see quota.rego).
type quotaChecker struct {
	subnetQuery           rego.PreparedEvalQuery
	networkInterfaceQuery rego.PreparedEvalQuery
	ipReservationQuery    rego.PreparedEvalQuery
}

func newQuotaChecker(ctx context.Context) (*quotaChecker, error) {
	subnetQuery, err := rego.New(
		rego.Query("data.kyuusha.network.quota.allow_subnet"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare subnet quota policy: %w", err)
	}
	networkInterfaceQuery, err := rego.New(
		rego.Query("data.kyuusha.network.quota.allow_network_interface"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare network interface quota policy: %w", err)
	}
	ipReservationQuery, err := rego.New(
		rego.Query("data.kyuusha.network.quota.allow_ip_reservation"),
		rego.Module("quota.rego", quotaPolicySrc),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare ip reservation quota policy: %w", err)
	}
	return &quotaChecker{subnetQuery: subnetQuery, networkInterfaceQuery: networkInterfaceQuery, ipReservationQuery: ipReservationQuery}, nil
}

func (q *quotaChecker) allowIPReservation(ctx context.Context, usage tenantUsage, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{"ip_reservation_count": usage.IPReservationCount},
		"limit": map[string]any{"max_ip_reservations": limit.GetMaxIpReservations()},
	}
	return evalAllow(ctx, q.ipReservationQuery, input)
}

func (q *quotaChecker) allowSubnet(ctx context.Context, usage tenantUsage, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{"subnet_count": usage.SubnetCount},
		"limit": map[string]any{"max_subnets": limit.GetMaxSubnets()},
	}
	return evalAllow(ctx, q.subnetQuery, input)
}

func (q *quotaChecker) allowNetworkInterface(ctx context.Context, usage tenantUsage, limit *identityv1.QuotaSpec) (bool, error) {
	input := map[string]any{
		"usage": map[string]any{"network_interface_count": usage.NetworkInterfaceCount},
		"limit": map[string]any{"max_network_interfaces": limit.GetMaxNetworkInterfaces()},
	}
	return evalAllow(ctx, q.networkInterfaceQuery, input)
}

func evalAllow(ctx context.Context, query rego.PreparedEvalQuery, input map[string]any) (bool, error) {
	results, err := query.Eval(ctx, rego.EvalInput(input))
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
// any other Create-time input problem -- same as compute/block-storage/
// image's identical helper.
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
