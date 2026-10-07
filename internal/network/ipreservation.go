package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
	"github.com/kiyuta1230/kyuusha/internal/resource"
)

// IPReservation holds Subnet addresses independently of any VM's
// NetworkInterface (a load balancer VIP, an external IP, ...). See
// proto/kyuusha/network/v1/ipreservation.proto and docs/specs/network.md
// 「IPReservation」. Addresses come from the same ipPool as
// NetworkInterface addresses, allocated by network-reconciler (like
// tryAllocateIP); kyuusha never wires them anywhere and they are never in
// a SecurityGroup address set.

var (
	ErrIPReservationNotFound      = errors.New("ip_reservation: not found")
	ErrIPReservationConflict      = errors.New("ip_reservation: resource_version conflict")
	ErrIPReservationHistoryPruned = errors.New("ip_reservation: watch resume point too old, relist required")
)

type IPReservationPhase string

const (
	IPReservationPhasePending IPReservationPhase = "Pending"
	IPReservationPhaseReady   IPReservationPhase = "Ready"
)

type IPReservationSpec struct {
	// Either NetworkID+Zone (a Subnet is picked at allocation time) or
	// SubnetID (NetworkID/Zone are then filled in from it at Create).
	NetworkID string
	Zone      string
	SubnetID  string
	// RequestedAddresses are optional specific addresses, at most one per
	// address family (IPv4 only for now); they need SubnetID.
	RequestedAddresses []string
}

type IPReservationStatus struct {
	Phase      IPReservationPhase
	Conditions []resource.Condition
	Addresses  []string
	SubnetID   string
	Zone       string
}

type IPReservation struct {
	Meta   resource.ObjectMeta
	Spec   IPReservationSpec
	Status IPReservationStatus
}

type IPReservationEvent = resource.Event[IPReservation]

// validateRequestedAddresses checks shape only (where they must fall is
// checked against the Subnet).
func validateRequestedAddresses(addrs []string) error {
	seen := map[bool]bool{}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			return fmt.Errorf("%w: requested_addresses: %q is not an IP address", ErrValidation, a)
		}
		v4 := ip.To4() != nil
		if !v4 {
			return fmt.Errorf("%w: requested_addresses: %q: only IPv4 addresses can be reserved for now", ErrValidation, a)
		}
		if seen[v4] {
			return fmt.Errorf("%w: requested_addresses: at most one address per address family", ErrValidation)
		}
		seen[v4] = true
	}
	return nil
}

// checkAddressInSubnet reports whether addr is a reservable host address
// of sn: inside its IPv4 CIDR, not the network/broadcast address and not
// its gateway_ip. allocatable_ip_ranges don't apply: a reservation may
// deliberately take an address outside them (a VIP at a fixed spot).
func checkAddressInSubnet(sn Subnet, addr string) error {
	cidr, gw := sn.Status.IPv4()
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("%w: subnet %q has no IPv4 CIDR yet", ErrValidation, sn.Meta.ID)
	}
	ip := net.ParseIP(addr).To4()
	start, end, ok := hostRange(ipnet)
	if ip == nil || !ok || ipAfter(start, ip) || ipAfter(ip, end) {
		return fmt.Errorf("%w: %s is not a host address of subnet %q (%s)", ErrValidation, addr, sn.Meta.ID, cidr)
	}
	if addr == gw {
		return fmt.Errorf("%w: %s is subnet %q's gateway", ErrValidation, addr, sn.Meta.ID)
	}
	return nil
}

// addressesInUse is every address currently held in subnetID by a
// NetworkInterface or an IPReservation, read from etcd -- the API side's
// early, best-effort "already taken" answer (the reconciler's ipPool is
// the authority).
func (s *Service) addressesInUse(ctx context.Context, subnetID string) (map[string]bool, error) {
	out := map[string]bool{}
	ifaces, err := s.interfaces.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, n := range ifaces {
		if n.SubnetID() == subnetID && n.Status.IPAddress != "" {
			out[n.Status.IPAddress] = true
		}
	}
	rs, err := s.reservations.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Status.SubnetID == subnetID {
			for _, a := range r.Status.Addresses {
				out[a] = true
			}
		}
	}
	return out, nil
}

func (s *Service) CreateIPReservation(ctx context.Context, tenantID, name string, spec IPReservationSpec, md resource.Metadata) (*IPReservation, error) {
	if err := resource.ValidateMetadata(md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	if spec.SubnetID == "" && (spec.NetworkID == "" || spec.Zone == "") {
		return nil, fmt.Errorf("%w: spec.subnet_id, or spec.network_id and spec.zone, are required", ErrValidation)
	}
	if err := validateRequestedAddresses(spec.RequestedAddresses); err != nil {
		return nil, err
	}
	if len(spec.RequestedAddresses) > 0 && spec.SubnetID == "" {
		return nil, fmt.Errorf("%w: requested_addresses needs spec.subnet_id", ErrValidation)
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if existing, ok := s.reservations.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}

	if spec.SubnetID != "" {
		subnet, err := s.getSubnetForInterface(ctx, tenantID, spec.SubnetID)
		if err != nil {
			if errors.Is(err, ErrSubnetNotFound) {
				return nil, fmt.Errorf("%w: subnet_id %q does not exist", ErrValidation, spec.SubnetID)
			}
			return nil, err
		}
		if spec.NetworkID != "" && spec.NetworkID != subnet.Spec.NetworkID || spec.Zone != "" && spec.Zone != subnet.Spec.Zone {
			return nil, fmt.Errorf("%w: subnet %q is in network %q zone %q, not the requested network/zone", ErrValidation, spec.SubnetID, subnet.Spec.NetworkID, subnet.Spec.Zone)
		}
		if subnet.Status.Phase != SubnetPhaseReady || subnet.Meta.DeletedAt != nil {
			return nil, fmt.Errorf("%w: subnet %q is not Ready", ErrValidation, spec.SubnetID)
		}
		spec.NetworkID, spec.Zone = subnet.Spec.NetworkID, subnet.Spec.Zone
		if len(spec.RequestedAddresses) > 0 {
			inUse, err := s.addressesInUse(ctx, subnet.Meta.ID)
			if err != nil {
				return nil, err
			}
			for _, a := range spec.RequestedAddresses {
				if err := checkAddressInSubnet(subnet, a); err != nil {
					return nil, err
				}
				if inUse[a] {
					return nil, fmt.Errorf("%w: %s is already in use in subnet %q", ErrValidation, a, subnet.Meta.ID)
				}
			}
		}
	}
	// Same rule as attaching a NetworkInterface: the caller's own Network,
	// or one shared with it / public.
	network, err := s.getNetworkAnyTenant(ctx, tenantID, spec.NetworkID)
	if err != nil {
		if errors.Is(err, ErrNetworkNotFound) {
			return nil, fmt.Errorf("%w: network_id %q does not exist", ErrValidation, spec.NetworkID)
		}
		return nil, err
	}
	if !network.UsableBy(tenantID) {
		return nil, fmt.Errorf("%w: network %q is not usable by tenant %q", ErrValidation, network.Meta.ID, tenantID)
	}
	if network.Meta.DeletedAt != nil {
		return nil, fmt.Errorf("%w: network %q is being deleted", ErrValidation, network.Meta.ID)
	}

	limit, err := lookupQuota(ctx, s.identityClient, tenantID)
	if err != nil {
		return nil, err
	}
	usage := s.usage[tenantID]
	allowed, err := s.quota.allowIPReservation(ctx, usage, limit)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("%w: tenant %q", ErrQuotaExceeded, tenantID)
	}

	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "CREATE", Resource: "IPReservation", TenantID: tenantID, Name: name,
		Labels: md.Labels, Annotations: md.Annotations, Spec: admissionIPReservationSpecJSON(spec),
	}); err != nil {
		return nil, err
	}
	out, err := s.reservations.Create(ctx, tenantID, name, IPReservation{
		Meta:   resource.ObjectMeta{Labels: md.Labels, Annotations: md.Annotations},
		Spec:   spec,
		Status: IPReservationStatus{Phase: IPReservationPhasePending},
	})
	if err != nil {
		return nil, err
	}
	usage.IPReservationCount++
	s.usage[tenantID] = usage
	return &out, nil
}

func (s *Service) GetIPReservation(ctx context.Context, tenantID, id string) (*IPReservation, error) {
	out, err := s.reservations.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListIPReservations with tenantID empty lists every tenant's.
func (s *Service) ListIPReservations(ctx context.Context, tenantID string) ([]IPReservation, error) {
	return s.reservations.List(ctx, tenantID)
}

func (s *Service) WatchIPReservations(ctx context.Context, tenantID string, sinceRV int64) (<-chan IPReservationEvent, error) {
	return s.reservations.Watch(ctx, tenantID, sinceRV, nil)
}

// UpdateIPReservation changes meta only (labels, annotations, finalizers);
// spec and status keep their stored values.
func (s *Service) UpdateIPReservation(ctx context.Context, tenantID string, r *IPReservation) (*IPReservation, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: r.Meta.Labels, Annotations: r.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if tenantID == "" || r.Meta.TenantID != tenantID {
		return nil, fmt.Errorf("%w: tenant_id must be set and match ip_reservation.meta.tenant_id", ErrValidation)
	}
	current, err := s.reservations.Get(ctx, tenantID, r.Meta.ID)
	if err != nil {
		return nil, err
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, r.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	r.Meta.Finalizers = finalizers
	r.Spec, r.Status = current.Spec, current.Status
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "UPDATE", Resource: "IPReservation", TenantID: tenantID, Name: current.Meta.Name, ID: current.Meta.ID,
		Labels: r.Meta.Labels, Annotations: r.Meta.Annotations, Spec: admissionIPReservationSpecJSON(r.Spec),
		OldObject: admissionIPReservationObject(current),
	}); err != nil {
		return nil, err
	}
	out, err := s.reservations.Update(ctx, *r)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteIPReservation mirrors DeleteNetworkInterface: Finalizers hold the
// reservation (and its addresses) in place; the addresses return to the
// pool on its Deleted event (see releaseIPReservation).
func (s *Service) DeleteIPReservation(ctx context.Context, tenantID, id string) error {
	if current, err := s.reservations.Get(ctx, tenantID, id); err == nil {
		if err := s.admit(ctx, admissionwebhook.Request{
			Operation: "DELETE", Resource: "IPReservation", TenantID: tenantID, Name: current.Meta.Name, ID: id,
			OldObject: admissionIPReservationObject(current),
		}); err != nil {
			return err
		}
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	r, err := s.reservations.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if err := s.reservations.Delete(ctx, tenantID, id); err != nil {
		return err
	}
	if r.Meta.DeletedAt != nil {
		return nil // already counted down by the first Delete call
	}
	usage := s.usage[tenantID]
	usage.IPReservationCount--
	s.usage[tenantID] = usage
	return nil
}

// reservationBlocking finds a reservation that keeps a Subnet (subnetID) or
// a Network (networkID) from being deleted.
func (s *Service) reservationBlocking(ctx context.Context, networkID, subnetID string) (*IPReservation, error) {
	all, err := s.reservations.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if networkID != "" && r.Spec.NetworkID == networkID ||
			subnetID != "" && (r.Spec.SubnetID == subnetID || r.Status.SubnetID == subnetID) {
			return &r, nil
		}
	}
	return nil, nil
}

// tryAllocateReservation gives r its address -- the requested one, or the
// first free one of a candidate Subnet, the same way tryAllocateIP does
// for a NetworkInterface -- and marks it Ready, or leaves it Pending with a
// NoFreeAddress condition. network-reconciler only.
func (s *Service) tryAllocateReservation(ctx context.Context, r *IPReservation) {
	if r.Meta.DeletedAt != nil || r.Status.Phase == IPReservationPhaseReady {
		return
	}
	candidates, err := s.candidateSubnetsFor(ctx, r.Meta.TenantID, r.Spec.SubnetID, r.Spec.NetworkID, r.Spec.Zone)
	if err != nil {
		return // transient; the sweep retries
	}
	for _, sn := range candidates {
		cidr, gw := sn.Status.IPv4()
		var ip string
		var ok bool
		if len(r.Spec.RequestedAddresses) > 0 {
			ip, ok = r.Spec.RequestedAddresses[0], s.ips.allocateSpecific(sn.Meta.ID, cidr, gw, r.Spec.RequestedAddresses[0])
		} else {
			ip, ok = s.ips.allocate(sn.Meta.ID, cidr, gw, sn.Spec.AllocatableIPRanges)
		}
		if !ok {
			continue
		}
		r.Status.Phase = IPReservationPhaseReady
		r.Status.Addresses = []string{ip}
		r.Status.SubnetID, r.Status.Zone = sn.Meta.ID, sn.Spec.Zone
		r.Status.Conditions = upsertCondition(r.Status.Conditions, resource.Condition{
			Type: "NoFreeAddress", Status: resource.ConditionFalse, LastTransitionAt: time.Now(),
		})
		updated, err := s.reservations.Update(ctx, *r)
		if err != nil {
			s.ips.release(sn.Meta.ID, ip)
			return
		}
		*r = updated
		return
	}
	msg := fmt.Sprintf("no Ready Subnet of network %q in zone %q has a free address", r.Spec.NetworkID, r.Spec.Zone)
	switch {
	case len(r.Spec.RequestedAddresses) > 0:
		msg = fmt.Sprintf("requested address %s of subnet %q is in use (or the subnet isn't Ready)", r.Spec.RequestedAddresses[0], r.Spec.SubnetID)
	case r.Spec.SubnetID != "":
		msg = fmt.Sprintf("subnet %q has no free address (or isn't Ready)", r.Spec.SubnetID)
	}
	for _, c := range r.Status.Conditions {
		if c.Type == "NoFreeAddress" && c.Status == resource.ConditionTrue && c.Message == msg {
			return // already says so: don't churn a Modified event every sweep
		}
	}
	r.Status.Conditions = upsertCondition(r.Status.Conditions, resource.Condition{
		Type: "NoFreeAddress", Status: resource.ConditionTrue, Message: msg, LastTransitionAt: time.Now(),
	})
	if updated, err := s.reservations.Update(ctx, *r); err == nil {
		*r = updated
	}
}

// releaseIPReservation returns r's addresses once it's actually gone (its
// Deleted event), never at Delete-call time -- see releaseSubnet.
func (s *Service) releaseIPReservation(r IPReservation) {
	for _, a := range r.Status.Addresses {
		s.ips.release(r.Status.SubnetID, a)
	}
}

func (s *Service) watchPendingIPReservations(ctx context.Context) {
	runWatchLoop(ctx, "ip reservations", func(ctx context.Context, rv int64) (<-chan IPReservationEvent, error) {
		return s.WatchIPReservations(ctx, "", rv)
	}, ErrIPReservationHistoryPruned, s.remarkPools, func(e IPReservationEvent) {
		if e.Type == EventDeleted {
			s.releaseIPReservation(e.Object)
			return
		}
		if e.Type != EventAdded || e.Object.Status.Phase != IPReservationPhasePending {
			return
		}
		r := e.Object
		s.tryAllocateReservation(ctx, &r)
	})
}

func (s *Service) retryPendingIPReservations(ctx context.Context) {
	all, err := s.reservations.List(ctx, "")
	if err != nil {
		return
	}
	for i := range all {
		if all[i].Status.Phase == IPReservationPhasePending {
			s.tryAllocateReservation(ctx, &all[i])
		}
	}
}

func (r *IPReservation) GetID() string                        { return r.Meta.ID }
func (r *IPReservation) SetID(id string)                      { r.Meta.ID = id }
func (r *IPReservation) GetName() string                      { return r.Meta.Name }
func (r *IPReservation) SetName(name string)                  { r.Meta.Name = name }
func (r *IPReservation) GetTenantID() string                  { return r.Meta.TenantID }
func (r *IPReservation) SetTenantID(id string)                { r.Meta.TenantID = id }
func (r *IPReservation) GetResourceVersion() int64            { return r.Meta.ResourceVersion }
func (r *IPReservation) SetResourceVersion(rv int64)          { r.Meta.ResourceVersion = rv }
func (r *IPReservation) GetCreatedAt() time.Time              { return r.Meta.CreatedAt }
func (r *IPReservation) SetCreatedAt(t time.Time)             { r.Meta.CreatedAt = t }
func (r *IPReservation) GetDeletedAt() *time.Time             { return r.Meta.DeletedAt }
func (r *IPReservation) SetDeletedAt(t *time.Time)            { r.Meta.DeletedAt = t }
func (r *IPReservation) GetFinalizers() []resource.Finalizer  { return r.Meta.Finalizers }
func (r *IPReservation) SetFinalizers(f []resource.Finalizer) { r.Meta.Finalizers = f }
