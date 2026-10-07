package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kiyuta1230/kyuusha/internal/admissionwebhook"
	"github.com/kiyuta1230/kyuusha/internal/resource"
	clientv3 "go.etcd.io/etcd/client/v3"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

var (
	ErrSubnetNotFound      = errors.New("subnet: not found")
	ErrSubnetConflict      = errors.New("subnet: resource_version conflict")
	ErrSubnetHistoryPruned = errors.New("subnet: watch resume point too old, relist required")

	ErrNetworkInterfaceNotFound      = errors.New("network_interface: not found")
	ErrNetworkInterfaceConflict      = errors.New("network_interface: resource_version conflict")
	ErrNetworkInterfaceHistoryPruned = errors.New("network_interface: watch resume point too old, relist required")

	ErrNetworkNotFound      = errors.New("network: not found")
	ErrNetworkConflict      = errors.New("network: resource_version conflict")
	ErrNetworkHistoryPruned = errors.New("network: watch resume point too old, relist required")

	ErrNetworkClassNotFound      = errors.New("network_class: not found")
	ErrNetworkClassConflict      = errors.New("network_class: resource_version conflict")
	ErrNetworkClassHistoryPruned = errors.New("network_class: watch resume point too old, relist required")

	ErrAllocationPoolNotFound      = errors.New("allocation_pool: not found")
	ErrAllocationPoolConflict      = errors.New("allocation_pool: resource_version conflict")
	ErrAllocationPoolHistoryPruned = errors.New("allocation_pool: watch resume point too old, relist required")

	// ErrInUse: deleting/shrinking something still referenced or allocated.
	ErrInUse = errors.New("network: still in use")

	ErrValidation    = errors.New("network: validation failed")
	ErrQuotaExceeded = errors.New("network: tenant quota exceeded")

	ErrAdmissionDenied      = errors.New("network: rejected by admission webhook")
	ErrAdmissionUnavailable = errors.New("network: admission webhook unavailable")
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

// Service implements the network service: AllocationPool/NetworkClass/
// Network/Subnet/NetworkInterface CRUD+Watch, with kyuusha's own
// allocation: a Network's and Subnet's values come from the pools its
// NetworkClass references (alloc.go), a NetworkInterface's IP from its
// Subnet's CIDR (ipam.go). Allocation never involves a hypervisor agent
// and never rejects a Create: the resource is created Pending, allocated
// by network-reconciler (Run), and left Pending with a Condition while a
// pool is exhausted, retried by Run's periodic sweep. See
// docs/specs/network.md.
type Service struct {
	subnets    *resource.Store[Subnet, *Subnet]
	interfaces *resource.Store[NetworkInterface, *NetworkInterface]
	networks   *resource.Store[Network, *Network]
	classes    *resource.Store[NetworkClass, *NetworkClass]
	pools      *resource.Store[AllocationPool, *AllocationPool]
	secgroups  *resource.Store[SecurityGroup, *SecurityGroup]

	alloc *allocator
	ips   *ipPool

	nextMACOct uint32

	// computeClient is used only by sweepOrphanedNetworkInterfaces, to ask
	// "does this NetworkInterface's vm_id still exist" -- the one place
	// this service needs to know anything about a VM at all.
	computeClient computev1.VirtualMachineServiceClient

	identityClient identityv1.TenantServiceClient
	quota          *quotaChecker

	usageMu sync.Mutex
	usage   map[string]tenantUsage

	// AdmissionGate is consulted synchronously before Subnet Create/Update/
	// Delete, NetworkInterface Create/Update/SetSecurityGroups and
	// SecurityGroup Create/Update/Delete (see
	// admit). Zero value (no URLs) allows everything; set by cmd/network's
	// -admission-webhook-urls.
	AdmissionGate admissionwebhook.Gate
}

// NewService constructs a Service and synchronously rebuilds its
// allocation/IP pools (see rebuildPools) and tenant quota usage (see rebuildUsage) from
// etcd before returning -- callers must not start serving Create requests
// until this returns, or a Create racing either rebuild could hand out an
// id/address/quota charge the rebuild was about to reserve.
func NewService(ctx context.Context, etcdClient *clientv3.Client, identityClient identityv1.TenantServiceClient, computeClient computev1.VirtualMachineServiceClient) (*Service, error) {
	quota, err := newQuotaChecker(ctx)
	if err != nil {
		return nil, err
	}
	svc := &Service{
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
		networks: resource.NewStore[Network, *Network](etcdClient, "network", resource.StoreErrors{
			NotFound:      ErrNetworkNotFound,
			Conflict:      ErrNetworkConflict,
			HistoryPruned: ErrNetworkHistoryPruned,
		}),
		classes: resource.NewStore[NetworkClass, *NetworkClass](etcdClient, "netclass", resource.StoreErrors{
			NotFound:      ErrNetworkClassNotFound,
			Conflict:      ErrNetworkClassConflict,
			HistoryPruned: ErrNetworkClassHistoryPruned,
		}),
		pools: resource.NewStore[AllocationPool, *AllocationPool](etcdClient, "allocpool", resource.StoreErrors{
			NotFound:      ErrAllocationPoolNotFound,
			Conflict:      ErrAllocationPoolConflict,
			HistoryPruned: ErrAllocationPoolHistoryPruned,
		}),
		secgroups: resource.NewStore[SecurityGroup, *SecurityGroup](etcdClient, "secgroup", resource.StoreErrors{
			NotFound:      ErrSecurityGroupNotFound,
			Conflict:      ErrSecurityGroupConflict,
			HistoryPruned: ErrSecurityGroupHistoryPruned,
		}),
		computeClient:  computeClient,
		identityClient: identityClient,
		quota:          quota,
		alloc:          newAllocator(),
		ips:            newIPPool(),
		usage:          make(map[string]tenantUsage),
	}
	if err := svc.rebuildPools(ctx); err != nil {
		return nil, err
	}
	if err := svc.rebuildUsage(ctx); err != nil {
		return nil, err
	}
	return svc, nil
}

// rebuildUsage restores usage's in-memory per-tenant quota accounting from
// every existing Subnet/NetworkInterface in etcd -- same bug class as
// rebuildPools/compute/block-storage/image's identical rebuilds: usage is
// purely in-memory, populated only by Create/Delete calls made within this
// process's own lifetime, so without this, every restart forgets every
// tenant's real usage. Neither Subnet nor NetworkInterface has Finalizer/
// soft-delete support (DeleteSubnet/DeleteNetworkInterface are direct hard
// deletes), so every object List returns is live -- no DeletedAt check
// needed here, unlike VirtualMachine/Volume's rebuilds.
func (s *Service) rebuildUsage(ctx context.Context) error {
	subnets, err := s.subnets.List(ctx, "")
	if err != nil {
		return fmt.Errorf("network: rebuild usage: list subnets: %w", err)
	}
	usage := make(map[string]tenantUsage)
	for _, sn := range subnets {
		u := usage[sn.Meta.TenantID]
		u.SubnetCount++
		usage[sn.Meta.TenantID] = u
	}
	ifaces, err := s.interfaces.List(ctx, "")
	if err != nil {
		return fmt.Errorf("network: rebuild usage: list network interfaces: %w", err)
	}
	for _, n := range ifaces {
		u := usage[n.Meta.TenantID]
		u.NetworkInterfaceCount++
		usage[n.Meta.TenantID] = u
	}
	s.usage = usage
	return nil
}

// rebuildPools restores alloc/ips/nextMACOct's in-memory allocation state
// from every existing Network/Subnet/NetworkInterface in etcd. Without this,
// allocator/ipPool/nextMACOct -- all purely in-memory, populated only by
// allocations made within this process's own lifetime -- forget every VLAN
// ID/IP address/MAC address already allocated on EVERY restart of this
// process, not just a hypothetical multi-replica scenario: a plain
// crash-and-restart is enough. The next allocation could then hand out an
// id/address already live on an existing Subnet/NetworkInterface (found
// live 2026-09-13: nextMACOct resetting to 0 meant the very first
// NetworkInterface created after any restart got the exact same MAC
// address as the very first one ever created, process-wide) -- a real
// conflict this rebuild prevents by seeding the in-memory state with
// reality before accepting any new request.
func (s *Service) rebuildPools(ctx context.Context) error {
	subnets, err := s.subnets.List(ctx, "")
	if err != nil {
		return fmt.Errorf("network: rebuild pools: list subnets: %w", err)
	}
	for _, sn := range subnets {
		for _, al := range sn.Status.Allocations {
			s.alloc.markUsed(al)
		}
	}
	networks, err := s.networks.List(ctx, "")
	if err != nil {
		return fmt.Errorf("network: rebuild pools: list networks: %w", err)
	}
	for _, n := range networks {
		for _, al := range n.Status.Allocations {
			s.alloc.markUsed(al)
		}
	}
	ifaces, err := s.interfaces.List(ctx, "")
	if err != nil {
		return fmt.Errorf("network: rebuild pools: list network interfaces: %w", err)
	}
	var maxMACOct uint32
	for _, n := range ifaces {
		if n.Status.IPAddress != "" {
			s.ips.markUsed(n.SubnetID(), n.Status.IPAddress)
		}
		if oct, ok := parseMACOct(n.Status.MACAddress); ok && oct > maxMACOct {
			maxMACOct = oct
		}
	}
	// Only ever raise the counter: remarkPools re-runs this on a live
	// process, where lowering it would re-issue MACs already handed out.
	for {
		cur := atomic.LoadUint32(&s.nextMACOct)
		if maxMACOct <= cur || atomic.CompareAndSwapUint32(&s.nextMACOct, cur, maxMACOct) {
			break
		}
	}
	return nil
}

// remarkPools is runWatchLoop's onPruned for network-reconciler's watches:
// re-marks every pool allocation/IP currently in etcd as used. It deliberately
// never frees anything -- an allocation already taken from the pool but
// not yet persisted would look free in a fresh listing, and freeing it
// could hand the same VLAN ID/IP out twice. So a deletion that happened
// inside the compacted gap stays unreturned until the next restart (an
// accepted, rare leak), but nothing is ever double-allocated.
func (s *Service) remarkPools(ctx context.Context) {
	if err := s.rebuildPools(ctx); err != nil {
		slog.Error("network: re-mark pools after watch history was pruned failed", "err", err)
	}
}

// parseMACOct extracts allocateMAC's counter value back out of a MAC
// address it produced ("02:00:00:00:<hi>:<lo>"), or ok=false if mac isn't
// in exactly that shape (e.g. empty, for a NetworkInterface that never got
// past Pending before this Status field was ever set).
func parseMACOct(mac string) (n uint32, ok bool) {
	parts := strings.Split(mac, ":")
	if len(parts) != 6 || parts[0] != "02" || parts[1] != "00" || parts[2] != "00" || parts[3] != "00" {
		return 0, false
	}
	hi, err := strconv.ParseUint(parts[4], 16, 8)
	if err != nil {
		return 0, false
	}
	lo, err := strconv.ParseUint(parts[5], 16, 8)
	if err != nil {
		return 0, false
	}
	return uint32(hi)<<8 | uint32(lo), true
}

// Run is network's reconcile loop: allocates vlan_id/ip_address/mac_address
// for Subnets/NetworkInterfaces CreateSubnet/CreateNetworkInterface leave
// Pending (see their doc comments -- this is the only place that ever
// touches vlanPool/ipPool/nextMACOct), and sweeps orphaned
// NetworkInterfaces every orphanSweepInterval. Safe to call from only one
// goroutine, and -- like compute-reconciler -- from only one process at a
// time (see docs/architecture.md "コントロールプレーンサービス自体の可用性"):
// cmd/network-reconciler calls this; cmd/network (the gRPC API) never
// does. Blocks until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	go s.watchPendingNetworks(ctx)
	go s.watchPendingSubnets(ctx)
	go s.watchPendingNetworkInterfaces(ctx)
	go s.watchVMPlacement(ctx)

	ticker := time.NewTicker(pendingSweepInterval)
	defer ticker.Stop()
	orphanTicker := time.NewTicker(orphanSweepInterval)
	defer orphanTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.retryPendingNetworks(ctx)
			s.retryPendingSubnets(ctx)
			s.retryPendingNetworkInterfaces(ctx)
		case <-orphanTicker.C:
			s.sweepOrphanedNetworkInterfaces(ctx)
		}
	}
}

// watchPendingSubnets attempts vlan_id allocation immediately when a
// Subnet is created while still Pending, instead of waiting up to
// pendingSweepInterval for the periodic sweep to notice it -- mirrors
// compute.Reconciler.Run's identical "the common, successful case
// shouldn't have to wait for a sweep tick" reasoning. Deliberately reacts
// to EventAdded only, not EventModified: tryAllocateVLAN's own Update call
// on a failed attempt is itself a Modified event, and reacting to that too
// would retry as fast as etcd round-trips complete instead of waiting for
// capacity to actually free up elsewhere -- retryPendingSubnets' periodic
// sweep is the right (and sufficient) backstop for that case.
func (s *Service) watchPendingSubnets(ctx context.Context) {
	runWatchLoop(ctx, "subnets", func(ctx context.Context, rv int64) (<-chan SubnetEvent, error) { return s.WatchSubnets(ctx, "", rv) }, ErrSubnetHistoryPruned, s.remarkPools, func(e SubnetEvent) {
		if e.Type == EventDeleted {
			s.releaseSubnet(e.Object)
			return
		}
		if e.Type != EventAdded || e.Object.Status.Phase != SubnetPhasePending {
			return
		}
		sn := e.Object
		s.tryAllocateSubnet(ctx, &sn)
	})
}

// releaseSubnet/releaseNetworkInterface return an object's pool values/IP to
// this process's pool once the object is actually gone (its Deleted
// event), never at Delete-call time: a Subnet/NetworkInterface held by a
// Finalizer lingers with deleted_at set and must keep its VLAN ID/IP until
// then, or they could be handed to something else while it still exists.
// Only network-reconciler allocates, so only its own pool's release
// matters -- which is why this hangs off its watch rather than off the
// Delete RPC (served by the API binary, whose pool is never allocated
// from). A deletion landing between NewService's rebuildPools and this
// watch starting is missed until the next restart; the window is the few
// milliseconds of startup.
func (s *Service) releaseSubnet(sn Subnet) {
	s.alloc.release(sn.Status.Allocations)
}

func (s *Service) releaseNetworkInterface(n NetworkInterface) {
	if n.Status.IPAddress != "" {
		s.ips.release(n.SubnetID(), n.Status.IPAddress)
	}
}

// watchPendingNetworkInterfaces mirrors watchPendingSubnets, for
// ip_address/mac_address allocation via tryAllocateIP -- see its doc
// comment for why only EventAdded triggers an immediate attempt.
func (s *Service) watchPendingNetworkInterfaces(ctx context.Context) {
	runWatchLoop(ctx, "network interfaces", func(ctx context.Context, rv int64) (<-chan NetworkInterfaceEvent, error) {
		return s.WatchNetworkInterfaces(ctx, "", rv)
	}, ErrNetworkInterfaceHistoryPruned, s.remarkPools, func(e NetworkInterfaceEvent) {
		if e.Type == EventDeleted {
			s.releaseNetworkInterface(e.Object)
			return
		}
		if e.Type != EventAdded || e.Object.Status.Phase != NetworkInterfacePhasePending {
			return
		}
		n := e.Object
		s.tryAllocateIP(ctx, &n)
	})
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
		vm, err := s.computeClient.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: iface.Meta.TenantID, Id: iface.Spec.VMID})
		if err == nil {
			if want := interfaceHypervisor(vm); iface.Status.Hypervisor != want {
				s.syncInterfaceHypervisor(ctx, iface.Meta.TenantID, iface.Spec.VMID, want)
			}
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
			s.tryAllocateSubnet(ctx, &subnets[i])
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
		s.tryAllocateIP(ctx, &ifaces[i])
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
	return s.CreateSubnetWithMetadata(ctx, tenantID, name, spec, resource.Metadata{})
}

// CreateSubnetWithMetadata is CreateSubnet that also sets meta.labels/
// annotations (see resource.Metadata). Like the spec, md is ignored when
// name matches an existing Subnet (the idempotent-retry path).
func (s *Service) CreateSubnetWithMetadata(ctx context.Context, tenantID, name string, spec SubnetSpec, md resource.Metadata) (*Subnet, error) {
	if err := resource.ValidateMetadata(md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if existing, ok := s.subnets.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil // idempotent retry: before validation, or the retry would clash with itself
	}
	if err := s.validateSubnetRequest(ctx, tenantID, spec); err != nil {
		return nil, err
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if existing, ok := s.subnets.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}

	limit, err := lookupQuota(ctx, s.identityClient, tenantID)
	if err != nil {
		return nil, err
	}
	usage := s.usage[tenantID]
	allowed, err := s.quota.allowSubnet(ctx, usage, limit)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("%w: tenant %q", ErrQuotaExceeded, tenantID)
	}

	// Always created Pending -- the actual allocation attempt happens only
	// in cmd/network-reconciler (see watchPendingSubnets/
	// retryPendingSubnets), never here. See docs/architecture.md
	// "コントロールプレーンサービス自体の可用性": the allocator is
	// process-local state, so this API handler must never touch it directly -- doing so would make it unsafe to run more than one
	// replica of this binary (each replica's own pool would drift from the
	// others' the moment either one allocates).
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "CREATE", Resource: "Subnet", TenantID: tenantID, Name: name,
		Labels: md.Labels, Annotations: md.Annotations, Spec: admissionSubnetSpecJSON(spec),
	}); err != nil {
		return nil, err
	}
	out, err := s.subnets.Create(ctx, tenantID, name, Subnet{
		Meta:   resource.ObjectMeta{Labels: md.Labels, Annotations: md.Annotations},
		Spec:   spec,
		Status: SubnetStatus{Phase: SubnetPhasePending},
	})
	if err != nil {
		return nil, err
	}

	usage.SubnetCount++
	s.usage[tenantID] = usage

	return &out, nil
}

// GetSubnet returns tenantID's own Subnet, or a Subnet of a Network shared
// with tenantID (or PUBLIC) -- the Subnets that tenant's NICs can land on.
// An empty tenantID (cross-tenant roles) reads any.
func (s *Service) GetSubnet(ctx context.Context, tenantID, id string) (*Subnet, error) {
	out, err := s.getSubnetForInterface(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if tenantID != "" && out.Meta.TenantID != tenantID {
		network, err := s.getNetworkAnyTenant(ctx, out.Meta.TenantID, out.Spec.NetworkID)
		if err != nil || !network.UsableBy(tenantID) {
			return nil, ErrSubnetNotFound
		}
	}
	return &out, nil
}

func (s *Service) ListSubnets(ctx context.Context, tenantID string) ([]Subnet, error) {
	return s.subnets.List(ctx, tenantID)
}

func (s *Service) UpdateSubnet(ctx context.Context, subnet *Subnet) (*Subnet, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: subnet.Meta.Labels, Annotations: subnet.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	current, err := s.subnets.Get(ctx, subnet.Meta.TenantID, subnet.Meta.ID)
	if err != nil {
		return nil, err
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, subnet.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	subnet.Meta.Finalizers = finalizers
	// network_id/zone/requested_addresses are fixed at Create: every
	// allocated value came from them, and every NetworkInterface's IP and
	// every guest's and host bridge's gateway from the resulting
	// addresses -- changing them under existing allocations would leave
	// addresses outside the CIDR or values from the wrong pools.
	if subnet.Spec.NetworkID != current.Spec.NetworkID || subnet.Spec.Zone != current.Spec.Zone || !slices.Equal(subnet.Spec.RequestedAddresses, current.Spec.RequestedAddresses) {
		return nil, fmt.Errorf("%w: spec.network_id/zone/requested_addresses cannot be changed after Create", ErrValidation)
	}
	if cidr, _ := current.Status.IPv4(); cidr != "" {
		if err := validateAllocatableIPRanges(cidr, subnet.Spec.AllocatableIPRanges); err != nil {
			return nil, fmt.Errorf("%w: spec.allocatable_ip_ranges: %v", ErrValidation, err)
		}
	}
	if err := validateIPs("spec.dns_servers", subnet.Spec.DNSServers); err != nil {
		return nil, err
	}
	// status is server-owned (allocated values above all: a caller-chosen
	// vlan_id would wire this tenant's VMs into another tenant's VLAN). This method
	// only serves the Update RPC; internal writers go through s.subnets.
	subnet.Status = current.Status
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "UPDATE", Resource: "Subnet", TenantID: current.Meta.TenantID, Name: current.Meta.Name, ID: current.Meta.ID,
		Labels: subnet.Meta.Labels, Annotations: subnet.Meta.Annotations, Spec: admissionSubnetSpecJSON(subnet.Spec),
		OldObject: admissionSubnetObject(current),
	}); err != nil {
		return nil, err
	}
	out, err := s.subnets.Update(ctx, *subnet)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSubnet deletes the Subnet -- or, if it carries Finalizers, marks it
// deleted_at and leaves it in place until they're all removed via Update
// (see resource.Store's Delete). Its VLAN ID returns to the pool only once
// it's actually gone (releaseSubnet). tenant_usage, by contrast, drops at
// the first Delete call, the same "Delete was requested" approximation
// compute.Service.Delete documents for VirtualMachine; a repeated Delete
// while Finalizers are pending doesn't drop it again.
func (s *Service) DeleteSubnet(ctx context.Context, tenantID, id string) error {
	// Admission runs before taking usageMu: a webhook round trip must not
	// hold up every other tenant's Create/Delete.
	if current, err := s.subnets.Get(ctx, tenantID, id); err == nil {
		if err := s.admit(ctx, admissionwebhook.Request{
			Operation: "DELETE", Resource: "Subnet", TenantID: tenantID, Name: current.Meta.Name, ID: id,
			OldObject: admissionSubnetObject(current),
		}); err != nil {
			return err
		}
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	sn, err := s.subnets.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if err := s.subnets.Delete(ctx, tenantID, id); err != nil {
		return err
	}
	if sn.Meta.DeletedAt != nil {
		return nil // already counted down by the first Delete call
	}

	usage := s.usage[tenantID]
	usage.SubnetCount--
	s.usage[tenantID] = usage

	return nil
}

func (s *Service) WatchSubnets(ctx context.Context, tenantID string, sinceRV int64) (<-chan SubnetEvent, error) {
	return s.subnets.Watch(ctx, tenantID, sinceRV, nil)
}

// CreateNetworkInterface validates spec.subnet_id against an existing,
// Ready Subnet in the same tenant before creating anything -- the same
// "never create a resource that references something that can't back it"
// rule as compute's Image validation (see internal/compute/image.go).
func (s *Service) CreateNetworkInterface(ctx context.Context, tenantID, name string, spec NetworkInterfaceSpec) (*NetworkInterface, error) {
	return s.CreateNetworkInterfaceWithMetadata(ctx, tenantID, name, spec, resource.Metadata{})
}

// CreateNetworkInterfaceWithMetadata is CreateNetworkInterface that also
// sets meta.labels/annotations -- see CreateSubnetWithMetadata.
func (s *Service) CreateNetworkInterfaceWithMetadata(ctx context.Context, tenantID, name string, spec NetworkInterfaceSpec, md resource.Metadata) (*NetworkInterface, error) {
	if err := resource.ValidateMetadata(md); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrValidation)
	}
	if spec.VMID == "" {
		return nil, fmt.Errorf("%w: spec.vm_id is required", ErrValidation)
	}
	if spec.SubnetID == "" && (spec.NetworkID == "" || spec.Zone == "") {
		return nil, fmt.Errorf("%w: spec.subnet_id, or spec.network_id and spec.zone, are required", ErrValidation)
	}
	if err := s.validateAttachSecurityGroups(ctx, tenantID, spec.SecurityGroupIDs); err != nil {
		return nil, err
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if existing, ok := s.interfaces.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}

	// A pinned Subnet decides network_id/zone; otherwise a Subnet is picked
	// at allocation time (tryAllocateIP), and none need exist yet -- the
	// interface just waits Pending for one, same as for a free address.
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
		if subnet.Status.Phase != SubnetPhaseReady {
			return nil, fmt.Errorf("%w: subnet %q is not Ready (phase=%s)", ErrValidation, spec.SubnetID, subnet.Status.Phase)
		}
		if subnet.Meta.DeletedAt != nil {
			return nil, fmt.Errorf("%w: subnet %q is being deleted", ErrValidation, spec.SubnetID)
		}
		spec.NetworkID, spec.Zone = subnet.Spec.NetworkID, subnet.Spec.Zone
	}
	// Network.UsableBy is only ever checked here, at attach time -- see
	// getSubnetForInterface's doc comment for why it doesn't gate on it.
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
	if len(spec.SecurityGroupIDs) == 0 {
		if network.Status.DefaultSecurityGroupID == "" {
			return nil, fmt.Errorf("%w: network %q has no default security group yet (not Ready?); name security_group_ids explicitly or retry", ErrValidation, network.Meta.ID)
		}
		spec.SecurityGroupIDs = []string{network.Status.DefaultSecurityGroupID}
	}

	limit, err := lookupQuota(ctx, s.identityClient, tenantID)
	if err != nil {
		return nil, err
	}
	usage := s.usage[tenantID]
	allowed, err := s.quota.allowNetworkInterface(ctx, usage, limit)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("%w: tenant %q", ErrQuotaExceeded, tenantID)
	}

	// Always created Pending, with no mac_address/ip_address set yet --
	// nextMACOct is just as much process-local state as vlanPool/ipPool
	// (two replicas would independently hand out the same counter value),
	// so MAC assignment moves to cmd/network-reconciler too, folded into
	// tryAllocateIP (see its doc comment) rather than done here.
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "CREATE", Resource: "NetworkInterface", TenantID: tenantID, Name: name,
		Labels: md.Labels, Annotations: md.Annotations, Spec: admissionNetworkInterfaceSpecJSON(spec),
	}); err != nil {
		return nil, err
	}
	out, err := s.interfaces.Create(ctx, tenantID, name, NetworkInterface{
		Meta:   resource.ObjectMeta{Labels: md.Labels, Annotations: md.Annotations},
		Spec:   spec,
		Status: NetworkInterfaceStatus{Phase: NetworkInterfacePhasePending},
	})
	if err != nil {
		return nil, err
	}

	usage.NetworkInterfaceCount++
	s.usage[tenantID] = usage

	return &out, nil
}

// tryAllocateIP gives n an address -- from its pinned Subnet, or else from
// the first Ready Subnet of n's Network in n's zone that has one free
// (picking the Subnet inside the same step as the IP is what keeps "chose
// a Subnet that then ran out" from ever happening: ipPool lives in this
// process alone) -- and marks it Ready with status.subnet_id set. With no
// address anywhere it stays Pending with a NoFreeAddress condition: the
// cue to add a Subnet. Also assigns n's mac_address the first time it runs
// for n -- like every allocation, only ever here in cmd/network-
// reconciler, never in the (possibly multi-replica) API handler.
func (s *Service) tryAllocateIP(ctx context.Context, n *NetworkInterface) {
	if n.Meta.DeletedAt != nil {
		return // being deleted (held by a Finalizer): never give it an IP now
	}
	candidates, err := s.candidateSubnets(ctx, n)
	if err != nil {
		return // transient; the sweep retries
	}
	if n.Status.MACAddress == "" {
		n.Status.MACAddress = s.allocateMAC()
	}
	for _, sn := range candidates {
		cidr, gw := sn.Status.IPv4()
		ip, ok := s.ips.allocate(sn.Meta.ID, cidr, gw, sn.Spec.AllocatableIPRanges)
		if !ok {
			continue
		}
		n.Status.Phase = NetworkInterfacePhaseReady
		n.Status.IPAddress = ip
		n.Status.SubnetID = sn.Meta.ID
		n.Status.Conditions = upsertCondition(n.Status.Conditions, resource.Condition{
			Type: "NoFreeAddress", Status: resource.ConditionFalse, LastTransitionAt: time.Now(),
		})
		updated, err := s.interfaces.Update(ctx, *n)
		if err != nil {
			s.ips.release(sn.Meta.ID, ip)
			return
		}
		*n = updated
		return
	}
	msg := fmt.Sprintf("no Ready Subnet of network %q in zone %q has a free address", n.Spec.NetworkID, n.Spec.Zone)
	if n.Spec.SubnetID != "" {
		msg = fmt.Sprintf("subnet %q has no free address (or isn't Ready)", n.Spec.SubnetID)
	}
	n.Status.Conditions = upsertCondition(n.Status.Conditions, resource.Condition{
		Type: "NoFreeAddress", Status: resource.ConditionTrue, Message: msg, LastTransitionAt: time.Now(),
	})
	if updated, err := s.interfaces.Update(ctx, *n); err == nil {
		*n = updated
	}
}

// candidateSubnets lists where n's address may come from, in the order to
// try: just the pinned Subnet, or every Ready, not-being-deleted Subnet of
// n's Network in n's zone, oldest first.
func (s *Service) candidateSubnets(ctx context.Context, n *NetworkInterface) ([]Subnet, error) {
	if n.Spec.SubnetID != "" {
		sn, err := s.getSubnetForInterface(ctx, n.Meta.TenantID, n.Spec.SubnetID)
		if err != nil {
			return nil, err
		}
		if sn.Status.Phase != SubnetPhaseReady {
			return nil, nil
		}
		return []Subnet{sn}, nil
	}
	network, err := s.getNetworkAnyTenant(ctx, n.Meta.TenantID, n.Spec.NetworkID)
	if err != nil {
		return nil, err
	}
	all, err := s.subnets.List(ctx, network.Meta.TenantID)
	if err != nil {
		return nil, err
	}
	var out []Subnet
	for _, sn := range all {
		if sn.Spec.NetworkID == network.Meta.ID && sn.Spec.Zone == n.Spec.Zone && sn.Status.Phase == SubnetPhaseReady && sn.Meta.DeletedAt == nil {
			out = append(out, sn)
		}
	}
	slices.SortFunc(out, func(a, b Subnet) int { return a.Meta.CreatedAt.Compare(b.Meta.CreatedAt) })
	return out, nil
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

// UpdateNetworkInterface rejects any request whose spec.security_group_ids
// differ from the currently-stored value -- SetSecurityGroups is the only
// sanctioned path for changing them (see its own doc comment and the RPC's
// doc comment in the proto), since only that path validates the groups and
// notifies the owning hypervisor via NATS. Without this check, a caller
// could smuggle a change through here and silently desync the enforced
// host state from etcd.
func (s *Service) UpdateNetworkInterface(ctx context.Context, iface *NetworkInterface) (*NetworkInterface, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: iface.Meta.Labels, Annotations: iface.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	current, err := s.interfaces.Get(ctx, iface.Meta.TenantID, iface.Meta.ID)
	if err != nil {
		return nil, err
	}
	if !stringsEqual(current.Spec.SecurityGroupIDs, iface.Spec.SecurityGroupIDs) {
		return nil, fmt.Errorf("%w: security_group_ids can only be changed via SetSecurityGroups", ErrValidation)
	}
	finalizers, err := resource.CheckFinalizerMutation(ctx, current.Meta.Finalizers, iface.Meta.Finalizers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	iface.Meta.Finalizers = finalizers
	// Only meta is caller-settable here: spec.vm_id/subnet_id are fixed at
	// Create (the rules were already checked unchanged above) and status
	// is server-owned (a caller-chosen ip_address/mac_address would
	// defeat SNAP's anti-spoofing, which trusts them). This method only
	// serves the Update RPC; internal writers go through s.interfaces.
	if iface.Spec.VMID != current.Spec.VMID || iface.Spec.SubnetID != current.Spec.SubnetID || iface.Spec.NetworkID != current.Spec.NetworkID || iface.Spec.Zone != current.Spec.Zone {
		return nil, fmt.Errorf("%w: spec.vm_id/subnet_id/network_id/zone cannot be changed", ErrValidation)
	}
	iface.Spec = current.Spec
	iface.Status = current.Status
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "UPDATE", Resource: "NetworkInterface", TenantID: current.Meta.TenantID, Name: current.Meta.Name, ID: current.Meta.ID,
		Labels: iface.Meta.Labels, Annotations: iface.Meta.Annotations, Spec: admissionNetworkInterfaceSpecJSON(iface.Spec),
		OldObject: admissionNetworkInterfaceObject(current),
	}); err != nil {
		return nil, err
	}

	out, err := s.interfaces.Update(ctx, *iface)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// stringsEqual treats nil and empty-but-non-nil as equal (proto decoding
// of an empty repeated field can go either way depending on the call path).
func stringsEqual(a, b []string) bool {
	return len(a) == 0 && len(b) == 0 || slices.Equal(a, b)
}

// SetSecurityGroups replaces a NetworkInterface's attached groups
// wholesale (empty = none, deny all). Hosts pick the change up through
// their policy streams (see PolicyHub).
func (s *Service) SetSecurityGroups(ctx context.Context, tenantID, id string, ids []string) (*NetworkInterface, error) {
	if err := s.validateAttachSecurityGroups(ctx, tenantID, ids); err != nil {
		return nil, err
	}
	n, err := s.interfaces.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	old := n
	n.Spec.SecurityGroupIDs = ids
	if err := s.admit(ctx, admissionwebhook.Request{
		Operation: "UPDATE", Resource: "NetworkInterface", TenantID: n.Meta.TenantID, Name: n.Meta.Name, ID: n.Meta.ID,
		Labels: n.Meta.Labels, Annotations: n.Meta.Annotations, Spec: admissionNetworkInterfaceSpecJSON(n.Spec),
		OldObject: admissionNetworkInterfaceObject(old),
	}); err != nil {
		return nil, err
	}
	out, err := s.interfaces.Update(ctx, n)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteNetworkInterface mirrors DeleteSubnet: Finalizers hold the
// interface (and its IP, see releaseNetworkInterface) in place.
func (s *Service) DeleteNetworkInterface(ctx context.Context, tenantID, id string) error {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	n, err := s.interfaces.Get(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if err := s.interfaces.Delete(ctx, tenantID, id); err != nil {
		return err
	}
	if n.Meta.DeletedAt != nil {
		return nil // already counted down by the first Delete call
	}

	usage := s.usage[tenantID]
	usage.NetworkInterfaceCount--
	s.usage[tenantID] = usage

	return nil
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
