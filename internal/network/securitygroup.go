package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sort"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// See proto/kyuusha/network/v1/securitygroup.proto and
// docs/specs/network.md「SecurityGroup」.

var (
	ErrSecurityGroupNotFound      = errors.New("security_group: not found")
	ErrSecurityGroupConflict      = errors.New("security_group: resource_version conflict")
	ErrSecurityGroupHistoryPruned = errors.New("security_group: watch resume point too old, relist required")
)

// SelfSecurityGroup as a rule's peer security_group_id means the group the
// rule belongs to ("members of this group may reach each other").
const SelfSecurityGroup = "self"

// maxSecurityGroupsPerInterface bounds how many groups one
// NetworkInterface carries, keeping the merged rule list a host has to
// evaluate (and ebpf-snap's fixed-size rule map) bounded.
const maxSecurityGroupsPerInterface = 16

// SecurityGroupPeer has exactly one field set.
type SecurityGroupPeer struct {
	CIDR            string
	SecurityGroupID string
	NetworkID       string
}

type SecurityGroupRule struct {
	Protocol    string // "" (any) / tcp / udp / icmp
	PortRange   string // tcp/udp only; "" = all ports
	Peer        SecurityGroupPeer
	Description string
}

type SecurityGroupSpec struct {
	Description         string
	IngressRules        []SecurityGroupRule
	EgressRules         []SecurityGroupRule
	SharedWithTenantIDs []string
}

type SecurityGroupStatus struct {
	DefaultForNetworkID string
}

type SecurityGroup struct {
	Meta   resource.ObjectMeta
	Spec   SecurityGroupSpec
	Status SecurityGroupStatus
}

type SecurityGroupEvent = resource.Event[SecurityGroup]

// setName is the name hosts know an address set by (see
// docs/specs/snap.md): rules reference these instead of carrying the
// addresses themselves.
func setNameSG(id string) string      { return "sg:" + id }
func setNameNetwork(id string) string { return "network:" + id }

// peerSet is the address set a rule's peer refers to ("" for a CIDR peer).
func (r SecurityGroupRule) peerSet(groupID string) string {
	switch {
	case r.Peer.SecurityGroupID == SelfSecurityGroup:
		return setNameSG(groupID)
	case r.Peer.SecurityGroupID != "":
		return setNameSG(r.Peer.SecurityGroupID)
	case r.Peer.NetworkID != "":
		return setNameNetwork(r.Peer.NetworkID)
	}
	return ""
}

// referencedSets is every address set g's rules (either direction) refer to.
func (g SecurityGroup) referencedSets() []string {
	var out []string
	for _, rules := range [][]SecurityGroupRule{g.Spec.IngressRules, g.Spec.EgressRules} {
		for _, r := range rules {
			if set := r.peerSet(g.Meta.ID); set != "" && !slices.Contains(out, set) {
				out = append(out, set)
			}
		}
	}
	return out
}

// defaultSecurityGroupSpec is what every Network's default group starts
// with: everything in from the same Network, everything out.
func defaultSecurityGroupSpec(networkID string) SecurityGroupSpec {
	return SecurityGroupSpec{
		Description: "default group of network " + networkID,
		IngressRules: []SecurityGroupRule{
			{Peer: SecurityGroupPeer{NetworkID: networkID}, Description: "everything from the same Network"},
		},
		EgressRules: []SecurityGroupRule{
			{Peer: SecurityGroupPeer{CIDR: "0.0.0.0/0"}, Description: "everything out (IPv4)"},
			{Peer: SecurityGroupPeer{CIDR: "::/0"}, Description: "everything out (IPv6)"},
		},
	}
}

func defaultSecurityGroupName(networkID string) string { return "default-" + networkID }

// validateSecurityGroupSpec checks spec's rules for a group owned by
// ownerTenantID: shape, plus that every group/Network a rule names exists
// and is one the owner may use.
func (s *Service) validateSecurityGroupSpec(ctx context.Context, ownerTenantID string, spec SecurityGroupSpec) error {
	for _, dir := range []struct {
		name  string
		rules []SecurityGroupRule
	}{{"ingress_rules", spec.IngressRules}, {"egress_rules", spec.EgressRules}} {
		for i, r := range dir.rules {
			if err := s.validateSecurityGroupRule(ctx, ownerTenantID, r); err != nil {
				return fmt.Errorf("%w: %s[%d]: %v", ErrValidation, dir.name, i, err)
			}
		}
	}
	for _, t := range spec.SharedWithTenantIDs {
		if t == "" {
			return fmt.Errorf("%w: shared_with_tenant_ids entries must be non-empty", ErrValidation)
		}
	}
	return nil
}

func (s *Service) validateSecurityGroupRule(ctx context.Context, ownerTenantID string, r SecurityGroupRule) error {
	switch r.Protocol {
	case "", "icmp":
		if r.PortRange != "" {
			return fmt.Errorf("port_range is only valid for tcp/udp")
		}
	case "tcp", "udp":
		if r.PortRange != "" {
			if err := validatePortRange(r.PortRange); err != nil {
				return fmt.Errorf("port_range %q: %v", r.PortRange, err)
			}
		}
	default:
		return fmt.Errorf("protocol %q must be empty (any), tcp, udp or icmp", r.Protocol)
	}
	set := 0
	for _, v := range []string{r.Peer.CIDR, r.Peer.SecurityGroupID, r.Peer.NetworkID} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("peer needs exactly one of cidr, security_group_id, network_id")
	}
	switch {
	case r.Peer.CIDR != "":
		if _, _, err := net.ParseCIDR(r.Peer.CIDR); err != nil {
			return fmt.Errorf("peer.cidr %q: %v", r.Peer.CIDR, err)
		}
	case r.Peer.SecurityGroupID == SelfSecurityGroup:
	case r.Peer.SecurityGroupID != "":
		g, err := s.getSecurityGroupAnyTenant(ctx, r.Peer.SecurityGroupID)
		if err != nil {
			if errors.Is(err, ErrSecurityGroupNotFound) {
				return fmt.Errorf("peer.security_group_id %q does not exist", r.Peer.SecurityGroupID)
			}
			return err
		}
		if !s.securityGroupUsableBy(ctx, g, ownerTenantID) {
			return fmt.Errorf("peer.security_group_id %q is not shared with tenant %q", r.Peer.SecurityGroupID, ownerTenantID)
		}
	case r.Peer.NetworkID != "":
		n, err := s.getNetworkAnyTenant(ctx, ownerTenantID, r.Peer.NetworkID)
		if err != nil {
			if errors.Is(err, ErrNetworkNotFound) {
				return fmt.Errorf("peer.network_id %q does not exist", r.Peer.NetworkID)
			}
			return err
		}
		if !n.UsableBy(ownerTenantID) {
			return fmt.Errorf("peer.network_id %q is not usable by tenant %q", r.Peer.NetworkID, ownerTenantID)
		}
	}
	return nil
}

// securityGroupUsableBy reports whether tenantID may attach g to its own
// NetworkInterfaces or name it as a peer: its owner, a tenant it's shared
// with, or -- for a Network's default group -- anyone who may use that
// Network (letting a tenant join a Network implies letting it join the
// Network's default group).
func (s *Service) securityGroupUsableBy(ctx context.Context, g SecurityGroup, tenantID string) bool {
	if g.Meta.TenantID == tenantID || slices.Contains(g.Spec.SharedWithTenantIDs, tenantID) {
		return true
	}
	if g.Status.DefaultForNetworkID == "" {
		return false
	}
	n, err := s.getNetworkAnyTenant(ctx, g.Meta.TenantID, g.Status.DefaultForNetworkID)
	return err == nil && n.UsableBy(tenantID)
}

func (s *Service) getSecurityGroupAnyTenant(ctx context.Context, id string) (SecurityGroup, error) {
	all, err := s.secgroups.List(ctx, "")
	if err != nil {
		return SecurityGroup{}, err
	}
	for _, g := range all {
		if g.Meta.ID == id {
			return g, nil
		}
	}
	return SecurityGroup{}, ErrSecurityGroupNotFound
}

func (s *Service) CreateSecurityGroup(ctx context.Context, tenantID, name string, spec SecurityGroupSpec, md resource.Metadata) (*SecurityGroup, error) {
	if err := resource.ValidateMetadata(md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if existing, ok := s.secgroups.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}
	if err := s.validateSecurityGroupSpec(ctx, tenantID, spec); err != nil {
		return nil, err
	}
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "CREATE", Resource: "SecurityGroup", TenantID: tenantID, Name: name,
		Labels: md.Labels, Annotations: md.Annotations, Spec: admissionSecurityGroupSpecJSON(spec),
	}); err != nil {
		return nil, err
	}
	out, err := s.secgroups.Create(ctx, tenantID, name, SecurityGroup{
		Meta: resource.ObjectMeta{Labels: md.Labels, Annotations: md.Annotations},
		Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSecurityGroup with tenantID empty returns any group; otherwise the
// caller's own, or another tenant's it may use (anything else reads as
// NotFound).
func (s *Service) GetSecurityGroup(ctx context.Context, tenantID, id string) (*SecurityGroup, error) {
	if tenantID != "" {
		if g, err := s.secgroups.Get(ctx, tenantID, id); err == nil {
			return &g, nil
		} else if !errors.Is(err, ErrSecurityGroupNotFound) {
			return nil, err
		}
	}
	g, err := s.getSecurityGroupAnyTenant(ctx, id)
	if err != nil {
		return nil, err
	}
	if tenantID != "" && !s.securityGroupUsableBy(ctx, g, tenantID) {
		return nil, ErrSecurityGroupNotFound
	}
	return &g, nil
}

// ListSecurityGroups with tenantID empty lists every tenant's groups;
// otherwise the caller's own plus those it may use.
func (s *Service) ListSecurityGroups(ctx context.Context, tenantID string) ([]SecurityGroup, error) {
	all, err := s.secgroups.List(ctx, "")
	if err != nil || tenantID == "" {
		return all, err
	}
	out := all[:0]
	for _, g := range all {
		if s.securityGroupUsableBy(ctx, g, tenantID) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *Service) WatchSecurityGroups(ctx context.Context, tenantID string, sinceRV int64) (<-chan SecurityGroupEvent, error) {
	return s.secgroups.Watch(ctx, tenantID, sinceRV, nil)
}

// UpdateSecurityGroup replaces spec, labels and annotations; status is
// server-owned. Hosts pick the change up through their policy streams
// (see PolicyHub).
func (s *Service) UpdateSecurityGroup(ctx context.Context, tenantID string, g *SecurityGroup) (*SecurityGroup, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: g.Meta.Labels, Annotations: g.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if tenantID == "" {
		tenantID = g.Meta.TenantID
	}
	if g.Meta.TenantID != tenantID {
		return nil, fmt.Errorf("%w: security_group.meta.tenant_id %q does not match tenant_id %q", ErrValidation, g.Meta.TenantID, tenantID)
	}
	current, err := s.secgroups.Get(ctx, tenantID, g.Meta.ID)
	if err != nil {
		return nil, err
	}
	if err := s.validateSecurityGroupSpec(ctx, tenantID, g.Spec); err != nil {
		return nil, err
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, g.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	g.Meta.Finalizers = finalizers
	g.Status = current.Status
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "UPDATE", Resource: "SecurityGroup", TenantID: tenantID, Name: current.Meta.Name, ID: current.Meta.ID,
		Labels: g.Meta.Labels, Annotations: g.Meta.Annotations, Spec: admissionSecurityGroupSpecJSON(g.Spec),
		OldObject: admissionSecurityGroupObject(current),
	}); err != nil {
		return nil, err
	}
	out, err := s.secgroups.Update(ctx, *g)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSecurityGroup refuses while anything still depends on the group:
// a NetworkInterface it's attached to, another group's rule naming it, or
// (for a default group) its Network.
func (s *Service) DeleteSecurityGroup(ctx context.Context, tenantID, id string) error {
	current, err := s.secgroups.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if netID := current.Status.DefaultForNetworkID; netID != "" {
		if _, err := s.networks.Get(ctx, current.Meta.TenantID, netID); err == nil {
			return fmt.Errorf("%w: %q is the default group of network %q", ErrInUse, current.Meta.Name, netID)
		}
	}
	ifaces, err := s.interfaces.List(ctx, "")
	if err != nil {
		return err
	}
	for _, n := range ifaces {
		if slices.Contains(n.Spec.SecurityGroupIDs, id) {
			return fmt.Errorf("%w: attached to NetworkInterface %q (tenant %q)", ErrInUse, n.Meta.ID, n.Meta.TenantID)
		}
	}
	groups, err := s.secgroups.List(ctx, "")
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.Meta.ID != id && slices.Contains(g.referencedSets(), setNameSG(id)) {
			return fmt.Errorf("%w: referenced by a rule of security group %q (tenant %q)", ErrInUse, g.Meta.ID, g.Meta.TenantID)
		}
	}
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "DELETE", Resource: "SecurityGroup", TenantID: tenantID, Name: current.Meta.Name, ID: id,
		OldObject: admissionSecurityGroupObject(current),
	}); err != nil {
		return err
	}
	return s.secgroups.Delete(ctx, tenantID, id)
}

// ensureDefaultSecurityGroup creates n's default group if it doesn't exist
// yet (idempotent by name) and returns its id. Called by
// network-reconciler before it marks n Ready, so a Ready Network always
// has one.
func (s *Service) ensureDefaultSecurityGroup(ctx context.Context, n Network) (string, error) {
	g, err := s.secgroups.Create(ctx, n.Meta.TenantID, defaultSecurityGroupName(n.Meta.ID), SecurityGroup{
		Spec:   defaultSecurityGroupSpec(n.Meta.ID),
		Status: SecurityGroupStatus{DefaultForNetworkID: n.Meta.ID},
	})
	if err != nil {
		return "", err
	}
	return g.Meta.ID, nil
}

// deleteDefaultSecurityGroup removes a deleted Network's default group.
// Groups whose rules still name it just stop matching anything.
func (s *Service) deleteDefaultSecurityGroup(ctx context.Context, n Network) {
	id := n.Status.DefaultSecurityGroupID
	if id == "" {
		return
	}
	if err := s.secgroups.Delete(ctx, n.Meta.TenantID, id); err != nil && !errors.Is(err, ErrSecurityGroupNotFound) {
		slog.Warn("network: delete default security group failed", "network_id", n.Meta.ID, "security_group_id", id, "err", err)
	}
}

// validateAttachSecurityGroups checks ids as a NetworkInterface of
// tenantID's attachment list.
func (s *Service) validateAttachSecurityGroups(ctx context.Context, tenantID string, ids []string) error {
	if len(ids) > maxSecurityGroupsPerInterface {
		return fmt.Errorf("%w: at most %d security groups per network interface", ErrValidation, maxSecurityGroupsPerInterface)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("%w: security group %q listed twice", ErrValidation, id)
		}
		seen[id] = true
		g, err := s.getSecurityGroupAnyTenant(ctx, id)
		if err != nil {
			if errors.Is(err, ErrSecurityGroupNotFound) {
				return fmt.Errorf("%w: security group %q does not exist", ErrValidation, id)
			}
			return err
		}
		if g.Meta.DeletedAt != nil {
			return fmt.Errorf("%w: security group %q is being deleted", ErrValidation, id)
		}
		if !s.securityGroupUsableBy(ctx, g, tenantID) {
			return fmt.Errorf("%w: security group %q is not shared with tenant %q", ErrValidation, id, tenantID)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// What a host enforces

// PolicyRule is one merged allow rule: exactly one of CIDR/Set.
type PolicyRule struct {
	Protocol  string
	PortRange string
	CIDR      string
	Set       string
}

// AddressSet is a versioned snapshot of one named set's members. Version
// is the etcd revision the snapshot reflects, the same clock membership
// deltas carry (see PolicyHub), so a host can tell which is newer.
type AddressSet struct {
	Name    string
	Version int64
	Members []string
}

type SecurityPolicy struct {
	SecurityGroupIDs []string
	IngressRules     []PolicyRule
	EgressRules      []PolicyRule
	Sets             []AddressSet
}

// memberSets is every address set n's address belongs to: its Network's,
// and each attached group's. Only while its VM is Running somewhere
// (status.hypervisor set): an address joins when the VM comes up and
// leaves as soon as it stops or is deleted, rather than lingering until
// the interface itself is garbage-collected.
func memberSets(n NetworkInterface) []string {
	if n.Status.IPAddress == "" || n.Status.Hypervisor == "" || n.Meta.DeletedAt != nil {
		return nil
	}
	var out []string
	if n.Spec.NetworkID != "" {
		out = append(out, setNameNetwork(n.Spec.NetworkID))
	}
	for _, id := range n.Spec.SecurityGroupIDs {
		out = append(out, setNameSG(id))
	}
	return out
}

// mergeRules flattens the rules of groups into one deduplicated list per
// direction (allow-only, so order and duplicates carry no meaning).
func mergeRules(groups []SecurityGroup) (ingress, egress []PolicyRule, sets []string) {
	conv := func(g SecurityGroup, rules []SecurityGroupRule, into []PolicyRule) []PolicyRule {
		for _, r := range rules {
			pr := PolicyRule{Protocol: r.Protocol, PortRange: r.PortRange, CIDR: r.Peer.CIDR, Set: r.peerSet(g.Meta.ID)}
			if pr.Set != "" && !slices.Contains(sets, pr.Set) {
				sets = append(sets, pr.Set)
			}
			if !slices.Contains(into, pr) {
				into = append(into, pr)
			}
		}
		return into
	}
	for _, g := range groups {
		ingress = conv(g, g.Spec.IngressRules, ingress)
		egress = conv(g, g.Spec.EgressRules, egress)
	}
	return ingress, egress, sets
}

// SecurityPolicy is what n's host enforces: its groups' merged rules plus
// a snapshot of every set they reference. A group that no longer exists
// contributes nothing.
func (s *Service) SecurityPolicy(ctx context.Context, n NetworkInterface) (SecurityPolicy, error) {
	all, err := s.secgroups.List(ctx, "")
	if err != nil {
		return SecurityPolicy{}, err
	}
	byID := make(map[string]SecurityGroup, len(all))
	for _, g := range all {
		byID[g.Meta.ID] = g
	}
	var groups []SecurityGroup
	for _, id := range n.Spec.SecurityGroupIDs {
		if g, ok := byID[id]; ok {
			groups = append(groups, g)
		}
	}
	ingress, egress, sets := mergeRules(groups)
	out := SecurityPolicy{SecurityGroupIDs: n.Spec.SecurityGroupIDs, IngressRules: ingress, EgressRules: egress}
	if len(sets) == 0 {
		return out, nil
	}
	ifaces, rev, err := s.interfaces.ListWithRevision(ctx, "")
	if err != nil {
		return SecurityPolicy{}, err
	}
	members := setMembers(ifaces)
	for _, name := range sets {
		out.Sets = append(out.Sets, AddressSet{Name: name, Version: rev, Members: members[name]})
	}
	return out, nil
}

// setMembers maps every address set to its members' addresses (sorted).
func setMembers(ifaces []NetworkInterface) map[string][]string {
	out := map[string][]string{}
	for _, n := range ifaces {
		for _, set := range memberSets(n) {
			out[set] = append(out[set], n.Status.IPAddress)
		}
	}
	for _, m := range out {
		sort.Strings(m)
	}
	return out
}

func (g *SecurityGroup) GetID() string                        { return g.Meta.ID }
func (g *SecurityGroup) SetID(id string)                      { g.Meta.ID = id }
func (g *SecurityGroup) GetName() string                      { return g.Meta.Name }
func (g *SecurityGroup) SetName(name string)                  { g.Meta.Name = name }
func (g *SecurityGroup) GetTenantID() string                  { return g.Meta.TenantID }
func (g *SecurityGroup) SetTenantID(id string)                { g.Meta.TenantID = id }
func (g *SecurityGroup) GetResourceVersion() int64            { return g.Meta.ResourceVersion }
func (g *SecurityGroup) SetResourceVersion(rv int64)          { g.Meta.ResourceVersion = rv }
func (g *SecurityGroup) GetCreatedAt() time.Time              { return g.Meta.CreatedAt }
func (g *SecurityGroup) SetCreatedAt(t time.Time)             { g.Meta.CreatedAt = t }
func (g *SecurityGroup) GetDeletedAt() *time.Time             { return g.Meta.DeletedAt }
func (g *SecurityGroup) SetDeletedAt(t *time.Time)            { g.Meta.DeletedAt = t }
func (g *SecurityGroup) GetFinalizers() []resource.Finalizer  { return g.Meta.Finalizers }
func (g *SecurityGroup) SetFinalizers(f []resource.Finalizer) { g.Meta.Finalizers = f }
