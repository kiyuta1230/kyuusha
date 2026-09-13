package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiyuta1230/kyuusha/internal/resource"
	clientv3 "go.etcd.io/etcd/client/v3"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

var (
	ErrSubnetNotFound      = errors.New("subnet: not found")
	ErrSubnetConflict      = errors.New("subnet: resource_version conflict")
	ErrSubnetHistoryPruned = errors.New("subnet: watch resume point too old, relist required")

	ErrNetworkInterfaceNotFound      = errors.New("network_interface: not found")
	ErrNetworkInterfaceConflict      = errors.New("network_interface: resource_version conflict")
	ErrNetworkInterfaceHistoryPruned = errors.New("network_interface: watch resume point too old, relist required")

	ErrValidation = errors.New("network: validation failed")
)

type SubnetEvent = resource.Event[Subnet]
type NetworkInterfaceEvent = resource.Event[NetworkInterface]

const (
	EventAdded    = resource.EventAdded
	EventModified = resource.EventModified
	EventDeleted  = resource.EventDeleted
	EventBookmark = resource.EventBookmark
)

// pendingSweepInterval mirrors compute's identical constant (see
// internal/compute/reconciler.go): a Subnet/NetworkInterface that couldn't
// be allocated at Create time (pool exhausted) only gets retried when
// capacity frees up elsewhere -- a Delete releasing an ID/IP doesn't touch
// the Pending resource waiting for one, so nothing re-triggers it on its
// own. This periodic sweep is that retry.
const pendingSweepInterval = 10 * time.Second

// orphanSweepInterval implements docs/architecture.md's "孤児リソースGC"
// design decision (10-minute periodic sweep; a child checks its parent's
// existence via Get, deletes itself if NotFound) for NetworkInterface --
// see docs/specs/network.md's "NetworkInterfaceのオーファンGC": VM Delete
// (compute.Reconciler.releaseIfReserved) never touches the NetworkInterfaces
// a VM held, so without this they stay Bound forever after their VM is
// gone (a real resource leak, not just a design gap). Ten minutes, not
// pendingSweepInterval's ten seconds: this is a backstop for VM deletion
// never having reached this NetworkInterface at all (a dropped/failed
// fire-and-forget call, or a VM removed by some other means), not a
// latency-sensitive retry -- see releaseIfReserved's own doc comment for
// the active-deletion path this backstops.
const orphanSweepInterval = 10 * time.Minute

// Service implements the SubnetService/NetworkInterfaceService CRUD+Watch
// surface against in-memory resource.Stores, with real (if simple) IPAM:
// Subnet Create allocates a VLAN ID from a per-zone pool (docs/architecture.md
// "VLAN IDの払い出し"), NetworkInterface Create allocates an IP from its
// Subnet's own CIDR (see ipam.go). Neither involves a hypervisor agent --
// both are synchronous, in-memory pool operations. Pool exhaustion doesn't
// reject Create (the request itself is valid, capacity may free up later):
// the resource is created Pending with a Condition, and Run's periodic
// sweep retries it. No tap wiring exists yet -- see docs/specs/network.md
// for the current boundary and why that stays out of this service.
type Service struct {
	subnets    *resource.Store[Subnet, *Subnet]
	interfaces *resource.Store[NetworkInterface, *NetworkInterface]

	vlans *vlanPool
	ips   *ipPool

	nextMACOct uint32

	// computeClient is used only by sweepOrphanedNetworkInterfaces, to ask
	// "does this NetworkInterface's vm_id still exist" -- the one place
	// this service needs to know anything about a VM at all.
	computeClient computev1.VirtualMachineServiceClient
}

func NewService(etcdClient *clientv3.Client, computeClient computev1.VirtualMachineServiceClient) *Service {
	return &Service{
		subnets: resource.NewStore[Subnet, *Subnet](etcdClient, "subnet", resource.StoreErrors{
			NotFound:      ErrSubnetNotFound,
			Conflict:      ErrSubnetConflict,
			HistoryPruned: ErrSubnetHistoryPruned,
		}),
		interfaces: resource.NewStore[NetworkInterface, *NetworkInterface](etcdClient, "netif", resource.StoreErrors{
			NotFound:      ErrNetworkInterfaceNotFound,
			Conflict:      ErrNetworkInterfaceConflict,
			HistoryPruned: ErrNetworkInterfaceHistoryPruned,
		}),
		computeClient: computeClient,
		vlans:         newVLANPool(),
		ips:           newIPPool(),
	}
}

// Run retries Pending Subnets/NetworkInterfaces (pool exhaustion at Create
// time) every pendingSweepInterval, and sweeps orphaned NetworkInterfaces
// every orphanSweepInterval (see that constant's doc comment), until ctx is
// done. Safe to call from only one goroutine; cmd/network/main.go starts it
// once at startup.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(pendingSweepInterval)
	defer ticker.Stop()
	orphanTicker := time.NewTicker(orphanSweepInterval)
	defer orphanTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.retryPendingSubnets(ctx)
			s.retryPendingNetworkInterfaces(ctx)
		case <-orphanTicker.C:
			s.sweepOrphanedNetworkInterfaces(ctx)
		}
	}
}

// sweepOrphanedNetworkInterfaces implements docs/architecture.md's
// orphan-GC detection logic (a child checks its own parent's existence via
// Get, deletes itself if NotFound) for NetworkInterface -- see
// orphanSweepInterval's doc comment for why this exists at all. Any error
// other than NotFound (compute unreachable, a transient RPC failure) is
// treated as "don't know, so don't delete" and just retried next tick --
// only a definitive NotFound is evidence of real orphaning.
func (s *Service) sweepOrphanedNetworkInterfaces(ctx context.Context) {
	if s.computeClient == nil {
		return // e.g. in tests that never set one
	}
	ifaces, err := s.interfaces.List(ctx, "")
	if err != nil {
		return
	}
	for _, iface := range ifaces {
		_, err := s.computeClient.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: iface.Meta.TenantID, Id: iface.Spec.VMID})
		if err == nil {
			continue
		}
		if status.Code(err) != codes.NotFound {
			slog.Warn("orphan sweep: could not confirm NetworkInterface's VM status, skipping this tick", "netif_id", iface.Meta.ID, "vm_id", iface.Spec.VMID, "err", err)
			continue
		}
		if derr := s.DeleteNetworkInterface(ctx, iface.Meta.TenantID, iface.Meta.ID); derr != nil {
			slog.Error("orphan sweep: delete orphaned NetworkInterface failed", "netif_id", iface.Meta.ID, "vm_id", iface.Spec.VMID, "err", derr)
			continue
		}
		slog.Info("orphan sweep: deleted NetworkInterface whose VM no longer exists", "netif_id", iface.Meta.ID, "vm_id", iface.Spec.VMID)
	}
}

func (s *Service) retryPendingSubnets(ctx context.Context) {
	subnets, err := s.subnets.List(ctx, "")
	if err != nil {
		return
	}
	for i := range subnets {
		if subnets[i].Status.Phase == SubnetPhasePending {
			s.tryAllocateVLAN(ctx, &subnets[i])
		}
	}
}

func (s *Service) retryPendingNetworkInterfaces(ctx context.Context) {
	ifaces, err := s.interfaces.List(ctx, "")
	if err != nil {
		return
	}
	for i := range ifaces {
		if ifaces[i].Status.Phase != NetworkInterfacePhasePending {
			continue
		}
		subnet, err := s.subnets.Get(ctx, ifaces[i].Meta.TenantID, ifaces[i].Spec.SubnetID)
		if err != nil || subnet.Status.Phase != SubnetPhaseReady {
			continue
		}
		s.tryAllocateIP(ctx, &ifaces[i], subnet.Spec.CIDR, subnet.Spec.GatewayIP, subnet.Spec.AllocatableIPRanges)
	}
}

// allocateMAC hands out a locally-administered MAC address from a global,
// unbounded counter -- unlike VLAN/IP, this never contends for a shared,
// exhaustible space scoped to a zone or Subnet, so there's nothing for
// IPAM to add here beyond this.
func (s *Service) allocateMAC() string {
	n := atomic.AddUint32(&s.nextMACOct, 1)
	return fmt.Sprintf("02:00:00:00:%02x:%02x", byte(n>>8), byte(n))
}

func (s *Service) CreateSubnet(ctx context.Context, tenantID, name string, spec SubnetSpec) (*Subnet, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if spec.Zone == "" {
		return nil, fmt.Errorf("%w: spec.zone is required", ErrValidation)
	}
	if _, _, err := net.ParseCIDR(spec.CIDR); err != nil {
		return nil, fmt.Errorf("%w: spec.cidr is invalid: %v", ErrValidation, err)
	}
	if spec.GatewayIP != "" && net.ParseIP(spec.GatewayIP) == nil {
		return nil, fmt.Errorf("%w: spec.gateway_ip is invalid", ErrValidation)
	}
	if err := validateAllocatableIPRanges(spec.CIDR, spec.AllocatableIPRanges); err != nil {
		return nil, fmt.Errorf("%w: spec.allocatable_ip_ranges: %v", ErrValidation, err)
	}

	if existing, ok := s.subnets.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}

	out, err := s.subnets.Create(ctx, tenantID, name, Subnet{
		Spec:   spec,
		Status: SubnetStatus{Phase: SubnetPhasePending},
	})
	if err != nil {
		return nil, err
	}
	s.tryAllocateVLAN(ctx, &out)
	return &out, nil
}

// tryAllocateVLAN attempts to allocate sn's VLAN ID from its zone's pool
// and persist the result, mutating sn in place either way: Ready+VlanID on
// success, still Pending with a VlanPoolExhausted condition on failure
// (retried later by Run's sweep). A store Update failure after a
// successful allocation rolls the allocation back, so it isn't leaked on a
// resource nobody ever sees as Ready.
func (s *Service) tryAllocateVLAN(ctx context.Context, sn *Subnet) {
	id, ok := s.vlans.allocate(sn.Spec.Zone)
	if !ok {
		sn.Status.Conditions = upsertCondition(sn.Status.Conditions, resource.Condition{
			Type: "VlanPoolExhausted", Status: resource.ConditionTrue, LastTransitionAt: time.Now(),
		})
		if updated, err := s.subnets.Update(ctx, *sn); err == nil {
			*sn = updated
		}
		return
	}

	sn.Status.Phase = SubnetPhaseReady
	sn.Status.VLANID = id
	sn.Status.Conditions = upsertCondition(sn.Status.Conditions, resource.Condition{
		Type: "VlanPoolExhausted", Status: resource.ConditionFalse, LastTransitionAt: time.Now(),
	})
	updated, err := s.subnets.Update(ctx, *sn)
	if err != nil {
		s.vlans.release(sn.Spec.Zone, id)
		return
	}
	*sn = updated
}

func (s *Service) GetSubnet(ctx context.Context, tenantID, id string) (*Subnet, error) {
	out, err := s.subnets.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListSubnets(ctx context.Context, tenantID string) ([]Subnet, error) {
	return s.subnets.List(ctx, tenantID)
}

func (s *Service) UpdateSubnet(ctx context.Context, subnet *Subnet) (*Subnet, error) {
	out, err := s.subnets.Update(ctx, *subnet)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSubnet releases the Subnet's VLAN ID back to its zone's pool first
// (if it ever held one -- a Subnet deleted while still Pending never did),
// mirroring compute's capacity-release-before-delete pattern.
func (s *Service) DeleteSubnet(ctx context.Context, tenantID, id string) error {
	if sn, err := s.subnets.Get(ctx, tenantID, id); err == nil && sn.Status.Phase == SubnetPhaseReady {
		s.vlans.release(sn.Spec.Zone, sn.Status.VLANID)
	}
	return s.subnets.Delete(ctx, tenantID, id)
}

func (s *Service) WatchSubnets(ctx context.Context, tenantID string, sinceRV int64) (<-chan SubnetEvent, error) {
	return s.subnets.Watch(ctx, tenantID, sinceRV, nil)
}

// CreateNetworkInterface validates spec.subnet_id against an existing,
// Ready Subnet in the same tenant before creating anything -- the same
// "never create a resource that references something that can't back it"
// rule as compute's Image validation (see internal/compute/image.go).
func (s *Service) CreateNetworkInterface(ctx context.Context, tenantID, name string, spec NetworkInterfaceSpec) (*NetworkInterface, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if spec.VMID == "" {
		return nil, fmt.Errorf("%w: spec.vm_id is required", ErrValidation)
	}
	if spec.SubnetID == "" {
		return nil, fmt.Errorf("%w: spec.subnet_id is required", ErrValidation)
	}

	if existing, ok := s.interfaces.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}

	subnet, err := s.subnets.Get(ctx, tenantID, spec.SubnetID)
	if err != nil {
		if errors.Is(err, ErrSubnetNotFound) {
			return nil, fmt.Errorf("%w: subnet_id %q does not exist", ErrValidation, spec.SubnetID)
		}
		return nil, err
	}
	if subnet.Status.Phase != SubnetPhaseReady {
		return nil, fmt.Errorf("%w: subnet %q is not Ready (phase=%s)", ErrValidation, spec.SubnetID, subnet.Status.Phase)
	}

	// MAC comes from its own unbounded space, so it's assigned up front
	// regardless of whether IP allocation below succeeds immediately.
	out, err := s.interfaces.Create(ctx, tenantID, name, NetworkInterface{
		Spec:   spec,
		Status: NetworkInterfaceStatus{Phase: NetworkInterfacePhasePending, MACAddress: s.allocateMAC()},
	})
	if err != nil {
		return nil, err
	}
	s.tryAllocateIP(ctx, &out, subnet.Spec.CIDR, subnet.Spec.GatewayIP, subnet.Spec.AllocatableIPRanges)
	return &out, nil
}

// tryAllocateIP mirrors tryAllocateVLAN: mutates n in place, Ready+IPAddress
// on success, still Pending with an IPPoolExhausted condition (retried
// later) if the Subnet's CIDR (or allocatableRanges, if set) has no free
// address left.
func (s *Service) tryAllocateIP(ctx context.Context, n *NetworkInterface, cidr, gatewayIP string, allocatableRanges []string) {
	ip, ok := s.ips.allocate(n.Spec.SubnetID, cidr, gatewayIP, allocatableRanges)
	if !ok {
		n.Status.Conditions = upsertCondition(n.Status.Conditions, resource.Condition{
			Type: "IPPoolExhausted", Status: resource.ConditionTrue, LastTransitionAt: time.Now(),
		})
		if updated, err := s.interfaces.Update(ctx, *n); err == nil {
			*n = updated
		}
		return
	}

	n.Status.Phase = NetworkInterfacePhaseReady
	n.Status.IPAddress = ip
	n.Status.Conditions = upsertCondition(n.Status.Conditions, resource.Condition{
		Type: "IPPoolExhausted", Status: resource.ConditionFalse, LastTransitionAt: time.Now(),
	})
	updated, err := s.interfaces.Update(ctx, *n)
	if err != nil {
		s.ips.release(n.Spec.SubnetID, ip)
		return
	}
	*n = updated
}

func (s *Service) GetNetworkInterface(ctx context.Context, tenantID, id string) (*NetworkInterface, error) {
	out, err := s.interfaces.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListNetworkInterfaces(ctx context.Context, tenantID string) ([]NetworkInterface, error) {
	return s.interfaces.List(ctx, tenantID)
}

func (s *Service) UpdateNetworkInterface(ctx context.Context, iface *NetworkInterface) (*NetworkInterface, error) {
	out, err := s.interfaces.Update(ctx, *iface)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteNetworkInterface releases the interface's IP back to its Subnet's
// pool first (if it ever held one), mirroring DeleteSubnet.
func (s *Service) DeleteNetworkInterface(ctx context.Context, tenantID, id string) error {
	if n, err := s.interfaces.Get(ctx, tenantID, id); err == nil && n.Status.Phase == NetworkInterfacePhaseReady && n.Status.IPAddress != "" {
		s.ips.release(n.Spec.SubnetID, n.Status.IPAddress)
	}
	return s.interfaces.Delete(ctx, tenantID, id)
}

func (s *Service) WatchNetworkInterfaces(ctx context.Context, tenantID string, sinceRV int64) (<-chan NetworkInterfaceEvent, error) {
	return s.interfaces.Watch(ctx, tenantID, sinceRV, nil)
}

// upsertCondition mirrors compute/reconciler.go's identical helper:
// Conditions accumulate history by type rather than growing unboundedly on
// every repeated retry.
func upsertCondition(conditions []resource.Condition, next resource.Condition) []resource.Condition {
	for i, c := range conditions {
		if c.Type == next.Type {
			conditions[i] = next
			return conditions
		}
	}
	return append(conditions, next)
}
