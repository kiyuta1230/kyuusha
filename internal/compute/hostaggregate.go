package compute

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// HostAggregate groups Hypervisors of one zone under labels -- placement
// narrower than a zone (e.g. the racks one VLAN reaches). See
// proto/kyuusha/compute/v1/hostaggregate.proto and
// docs/specs/vm-scheduling.md「HostAggregate」. Cluster-scoped
// (Meta.TenantID is always ""), admin-only.

var (
	ErrHostAggregateNotFound      = errors.New("host aggregate: not found")
	ErrHostAggregateConflict      = errors.New("host aggregate: resource_version conflict")
	ErrHostAggregateHistoryPruned = errors.New("host aggregate: watch resume point too old, relist required")
)

type HostAggregateSpec struct {
	Zone        string
	Labels      map[string]string
	Hypervisors []string
}

type HostAggregate struct {
	Meta resource.ObjectMeta
	Spec HostAggregateSpec
}

type HostAggregateEvent = resource.Event[HostAggregate]

func validateHostAggregateSpec(spec HostAggregateSpec) error {
	if spec.Zone == "" {
		return fmt.Errorf("%w: zone is required", ErrValidation)
	}
	for k := range spec.Labels {
		if k == "" {
			return fmt.Errorf("%w: label keys must be non-empty", ErrValidation)
		}
	}
	seen := make(map[string]bool, len(spec.Hypervisors))
	for _, h := range spec.Hypervisors {
		if h == "" {
			return fmt.Errorf("%w: hypervisors[] entries must be non-empty", ErrValidation)
		}
		if seen[h] {
			return fmt.Errorf("%w: hypervisor %q listed twice", ErrValidation, h)
		}
		seen[h] = true
	}
	return nil
}

// CreateHostAggregate doesn't require the member Hypervisors to exist yet:
// an operator may define the rack layout before the hosts register.
func (s *Service) CreateHostAggregate(ctx context.Context, name string, spec HostAggregateSpec) (*HostAggregate, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if err := validateHostAggregateSpec(spec); err != nil {
		return nil, err
	}
	out, err := s.aggregates.Create(ctx, "", name, HostAggregate{Spec: spec})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) GetHostAggregate(ctx context.Context, id string) (*HostAggregate, error) {
	out, err := s.aggregates.Get(ctx, "", id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListHostAggregates(ctx context.Context) ([]HostAggregate, error) {
	return s.aggregates.List(ctx, "")
}

func (s *Service) WatchHostAggregates(ctx context.Context, sinceRV int64) (<-chan HostAggregateEvent, error) {
	return s.aggregates.Watch(ctx, "", sinceRV, nil)
}

// UpdateHostAggregate replaces the spec. Like a NetworkClass change, it
// only affects later scheduling: VMs already placed stay where they are.
func (s *Service) UpdateHostAggregate(ctx context.Context, agg *HostAggregate) (*HostAggregate, error) {
	if err := validateHostAggregateSpec(agg.Spec); err != nil {
		return nil, err
	}
	current, err := s.aggregates.Get(ctx, "", agg.Meta.ID)
	if err != nil {
		return nil, err
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, agg.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	agg.Meta.Finalizers = finalizers
	out, err := s.aggregates.Update(ctx, *agg)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) DeleteHostAggregate(ctx context.Context, id string) error {
	return s.aggregates.Delete(ctx, "", id)
}

// aggregateSelectors resolves the host_aggregate_selector of every
// attachment's NetworkClass (Subnet -> Network -> NetworkClass), skipping
// empty ones. Without the Network/NetworkClass clients configured (most
// tests) there's no constraint.
func (s *Service) aggregateSelectors(ctx context.Context, tenantID string, attachments []NetworkAttachment) ([]map[string]string, error) {
	if s.NetworkClient == nil || s.NetworkClassClient == nil {
		return nil, nil
	}
	var out []map[string]string
	seen := map[string]bool{}
	for _, a := range attachments {
		networkID := a.NetworkID
		if networkID == "" && a.SubnetID != "" {
			sn, err := s.subnetClient.Get(ctx, &networkv1.GetSubnetRequest{TenantId: tenantID, Id: a.SubnetID})
			if err != nil {
				return nil, err
			}
			networkID = sn.GetSpec().GetNetworkId()
		}
		if networkID == "" || seen[networkID] {
			continue
		}
		seen[networkID] = true
		n, err := s.NetworkClient.Get(ctx, &networkv1.GetNetworkRequest{TenantId: tenantID, Id: networkID})
		if err != nil {
			return nil, err
		}
		c, err := s.NetworkClassClient.Get(ctx, &networkv1.GetNetworkClassRequest{Id: n.GetSpec().GetNetworkClass()})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil, fmt.Errorf("%w: network %q's class %q does not exist", ErrValidation, networkID, n.GetSpec().GetNetworkClass())
			}
			return nil, err
		}
		if sel := c.GetSpec().GetHostAggregateSelector(); len(sel) > 0 {
			out = append(out, sel)
		}
	}
	return out, nil
}

// matchesAggregateSelectors reports whether h satisfies every selector:
// for each, some aggregate in h's zone lists h and carries all of the
// selector's labels (different selectors may be met by different
// aggregates). No selectors is trivially satisfied.
func matchesAggregateSelectors(h Hypervisor, selectors []map[string]string, aggregates []HostAggregate) bool {
	for _, sel := range selectors {
		found := false
		for _, a := range aggregates {
			if a.Spec.Zone == h.Status.Zone && containsString(a.Spec.Hypervisors, h.Meta.ID) && hasLabels(a.Spec.Labels, sel) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (a *HostAggregate) GetID() string                        { return a.Meta.ID }
func (a *HostAggregate) SetID(id string)                      { a.Meta.ID = id }
func (a *HostAggregate) GetName() string                      { return a.Meta.Name }
func (a *HostAggregate) SetName(name string)                  { a.Meta.Name = name }
func (a *HostAggregate) GetTenantID() string                  { return a.Meta.TenantID }
func (a *HostAggregate) SetTenantID(id string)                { a.Meta.TenantID = id }
func (a *HostAggregate) GetResourceVersion() int64            { return a.Meta.ResourceVersion }
func (a *HostAggregate) SetResourceVersion(rv int64)          { a.Meta.ResourceVersion = rv }
func (a *HostAggregate) GetCreatedAt() time.Time              { return a.Meta.CreatedAt }
func (a *HostAggregate) SetCreatedAt(t time.Time)             { a.Meta.CreatedAt = t }
func (a *HostAggregate) GetDeletedAt() *time.Time             { return a.Meta.DeletedAt }
func (a *HostAggregate) SetDeletedAt(t *time.Time)            { a.Meta.DeletedAt = t }
func (a *HostAggregate) GetFinalizers() []resource.Finalizer  { return a.Meta.Finalizers }
func (a *HostAggregate) SetFinalizers(f []resource.Finalizer) { a.Meta.Finalizers = f }
