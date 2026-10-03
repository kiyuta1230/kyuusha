package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

	identityClient identityv1.TenantServiceClient
	quota          *quotaChecker

	usageMu sync.Mutex
	usage   map[string]tenantUsage

	// js is used only by UpdateFirewallRules's publishUpdateACL, to notify
	// whichever hypervisor is currently running a NetworkInterface's VM
	// that its ingress_rules/egress_rules changed. nil (e.g. cmd/network-
	// reconciler, which serves no UpdateFirewallRules RPC at all, and most
	// tests) makes publishUpdateACL a no-op -- best-effort, never blocks or
	// fails the RPC itself (see docs/specs/network.md「セキュリティ
	// バックエンド」).
	js jetstream.JetStream

	// AdmissionGate is consulted synchronously before Subnet Create/Update/
	// Delete and NetworkInterface Create/Update/UpdateFirewallRules (see
	// admit). Zero value (no URLs) allows everything; set by cmd/network's
	// -admission-webhook-urls.
	AdmissionGate admissionwebhook.Gate
}

// NewService constructs a Service and synchronously rebuilds its VLAN/IP
// pools (see rebuildPools) and tenant quota usage (see rebuildUsage) from
// etcd before returning -- callers must not start serving Create requests
// until this returns, or a Create racing either rebuild could hand out an
// id/address/quota charge the rebuild was about to reserve.
func NewService(ctx context.Context, etcdClient *clientv3.Client, identityClient identityv1.TenantServiceClient, computeClient computev1.VirtualMachineServiceClient, js jetstream.JetStream) (*Service, error) {
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
		computeClient:  computeClient,
		identityClient: identityClient,
		quota:          quota,
		js:             js,
		vlans:          newVLANPool(),
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

// SetVLANRanges restricts which VLAN IDs each zone hands out (see
// VLANRanges). Only cmd/network-reconciler allocates VLAN IDs, so only it
// needs to call this; call it before starting the allocation watch.
func (s *Service) SetVLANRanges(r VLANRanges) {
	s.vlans.setRanges(r)
}

// rebuildPools restores vlans/ips/nextMACOct's in-memory allocation state
// from every existing Subnet/NetworkInterface in etcd. Without this,
// vlanPool/ipPool/nextMACOct -- all purely in-memory, populated only by
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
		if sn.Status.VLANID != 0 {
			s.vlans.markUsed(sn.Spec.Zone, sn.Status.VLANID)
		}
	}
	ifaces, err := s.interfaces.List(ctx, "")
	if err != nil {
		return fmt.Errorf("network: rebuild pools: list network interfaces: %w", err)
	}
	var maxMACOct uint32
	for _, n := range ifaces {
		if n.Status.IPAddress != "" {
			s.ips.markUsed(n.Spec.SubnetID, n.Status.IPAddress)
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
// re-marks every VLAN ID/IP currently in etcd as used. It deliberately
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
		s.tryAllocateVLAN(ctx, &sn)
	})
}

// releaseSubnet/releaseNetworkInterface return an object's VLAN ID/IP to
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
	if sn.Status.VLANID != 0 {
		s.vlans.release(sn.Spec.Zone, sn.Status.VLANID)
	}
}

func (s *Service) releaseNetworkInterface(n NetworkInterface) {
	if n.Status.IPAddress != "" {
		s.ips.release(n.Spec.SubnetID, n.Status.IPAddress)
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
		subnet, err := s.getSubnetForInterface(ctx, n.Meta.TenantID, n.Spec.SubnetID)
		if err != nil || subnet.Status.Phase != SubnetPhaseReady {
			return // retryPendingNetworkInterfaces' sweep retries once the Subnet is Ready
		}
		s.tryAllocateIP(ctx, &n, subnet.Spec.CIDR, subnet.Spec.GatewayIP, subnet.Spec.AllocatableIPRanges)
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
		subnet, err := s.getSubnetForInterface(ctx, ifaces[i].Meta.TenantID, ifaces[i].Spec.SubnetID)
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
	if spec.UniqueCidr {
		if err := s.validateUniqueCIDR(ctx, "", spec.CIDR); err != nil {
			return nil, err
		}
	}
	if spec.Visibility == SubnetVisibilityPublic && !spec.UniqueCidr {
		return nil, fmt.Errorf("%w: spec.visibility=PUBLIC requires spec.unique_cidr=true (open, unvetted cross-tenant attach is only allowed on Public IP address space; a purposeful, owner-vetted grant to specific tenants should use spec.shared_with_tenant_ids instead, on any Subnet)", ErrValidation)
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

	// Always created Pending -- the actual vlan_id allocation attempt
	// happens only in cmd/network-reconciler (see
	// watchPendingSubnets/retryPendingSubnets), never here. See
	// docs/architecture.md "コントロールプレーンサービス自体の可用性":
	// vlanPool is process-local state, so this API handler must never touch
	// it directly -- doing so would make it unsafe to run more than one
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

// tryAllocateVLAN attempts to allocate sn's VLAN ID from its zone's pool
// and persist the result, mutating sn in place either way: Ready+VlanID on
// success, still Pending with a VlanPoolExhausted condition on failure
// (retried later by Run's sweep). A store Update failure after a
// successful allocation rolls the allocation back, so it isn't leaked on a
// resource nobody ever sees as Ready.
func (s *Service) tryAllocateVLAN(ctx context.Context, sn *Subnet) {
	if sn.Meta.DeletedAt != nil {
		return // being deleted (held by a Finalizer): never give it a VLAN ID now
	}
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
	if err := resource.ValidateMetadata(resource.Metadata{Labels: subnet.Meta.Labels, Annotations: subnet.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if subnet.Spec.UniqueCidr {
		if err := s.validateUniqueCIDR(ctx, subnet.Meta.ID, subnet.Spec.CIDR); err != nil {
			return nil, err
		}
	}
	if subnet.Spec.Visibility == SubnetVisibilityPublic && !subnet.Spec.UniqueCidr {
		return nil, fmt.Errorf("%w: spec.visibility=PUBLIC requires spec.unique_cidr=true (open, unvetted cross-tenant attach is only allowed on Public IP address space; a purposeful, owner-vetted grant to specific tenants should use spec.shared_with_tenant_ids instead, on any Subnet)", ErrValidation)
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
	// zone/cidr/gateway_ip are fixed at Create: the VLAN ID was allocated
	// from zone's pool, every NetworkInterface's IP from cidr, and every
	// running guest and host bridge is configured with gateway_ip --
	// changing any of them under existing allocations would leave
	// addresses outside the CIDR, a VLAN ID from the wrong zone's pool, or
	// a gateway colliding with an already-allocated IP.
	if subnet.Spec.Zone != current.Spec.Zone || subnet.Spec.CIDR != current.Spec.CIDR || subnet.Spec.GatewayIP != current.Spec.GatewayIP {
		return nil, fmt.Errorf("%w: spec.zone/cidr/gateway_ip cannot be changed after Create", ErrValidation)
	}
	// status is server-owned (vlan_id above all: a caller-chosen vlan_id
	// would wire this tenant's VMs into another tenant's VLAN). This method
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
	if spec.SubnetID == "" {
		return nil, fmt.Errorf("%w: spec.subnet_id is required", ErrValidation)
	}
	if err := validateFirewallRules(spec.IngressRules); err != nil {
		return nil, err
	}
	if err := validateFirewallRules(spec.EgressRules); err != nil {
		return nil, err
	}

	s.usageMu.Lock()
	defer s.usageMu.Unlock()

	if existing, ok := s.interfaces.LookupByName(ctx, tenantID, name); ok {
		return &existing, nil
	}

	subnet, err := s.getSubnetForInterface(ctx, tenantID, spec.SubnetID)
	if err != nil {
		if errors.Is(err, ErrSubnetNotFound) {
			return nil, fmt.Errorf("%w: subnet_id %q does not exist", ErrValidation, spec.SubnetID)
		}
		return nil, err
	}
	// subnetUsableBy is only ever checked here, at attach time -- see its
	// own doc comment for why getSubnetForInterface itself doesn't gate on
	// it.
	if !subnetUsableBy(subnet, tenantID) {
		return nil, fmt.Errorf("%w: subnet_id %q is not usable by tenant %q", ErrValidation, spec.SubnetID, tenantID)
	}
	if subnet.Status.Phase != SubnetPhaseReady {
		return nil, fmt.Errorf("%w: subnet %q is not Ready (phase=%s)", ErrValidation, spec.SubnetID, subnet.Status.Phase)
	}
	if subnet.Meta.DeletedAt != nil {
		return nil, fmt.Errorf("%w: subnet %q is being deleted", ErrValidation, spec.SubnetID)
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

// tryAllocateIP mirrors tryAllocateVLAN: mutates n in place, Ready+IPAddress
// on success, still Pending with an IPPoolExhausted condition (retried
// later) if the Subnet's CIDR (or allocatableRanges, if set) has no free
// address left. Also assigns n's mac_address the first time it runs for n
// (a no-op on a later retry, once already set) -- CreateNetworkInterface
// itself never does this (see its doc comment): unlike vlan_id/ip_address,
// mac_address allocation can't fail/exhaust, but nextMACOct is exactly as
// process-local as vlanPool/ipPool, so it still has to happen only here,
// in cmd/network-reconciler, not in the (possibly multi-replica) API
// handler.
func (s *Service) tryAllocateIP(ctx context.Context, n *NetworkInterface, cidr, gatewayIP string, allocatableRanges []string) {
	if n.Meta.DeletedAt != nil {
		return // being deleted (held by a Finalizer): never give it an IP now
	}
	if n.Status.MACAddress == "" {
		n.Status.MACAddress = s.allocateMAC()
	}

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

// UpdateNetworkInterface rejects any request whose spec.ingress_rules/
// egress_rules differ from the currently-stored value -- UpdateFirewallRules
// is the only sanctioned path for changing either list (see its own doc
// comment and the UpdateFirewallRules RPC's doc comment in the proto),
// since only that path validates the rules and notifies the owning
// hypervisor via NATS. Without this check, a caller could smuggle a rule
// change through here and silently desync the enforced host state from
// etcd.
func (s *Service) UpdateNetworkInterface(ctx context.Context, iface *NetworkInterface) (*NetworkInterface, error) {
	if err := resource.ValidateMetadata(resource.Metadata{Labels: iface.Meta.Labels, Annotations: iface.Meta.Annotations}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	current, err := s.interfaces.Get(ctx, iface.Meta.TenantID, iface.Meta.ID)
	if err != nil {
		return nil, err
	}
	if !firewallRulesEqual(current.Spec.IngressRules, iface.Spec.IngressRules) ||
		!firewallRulesEqual(current.Spec.EgressRules, iface.Spec.EgressRules) {
		return nil, fmt.Errorf("%w: ingress_rules/egress_rules can only be changed via UpdateFirewallRules", ErrValidation)
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
	if iface.Spec.VMID != current.Spec.VMID || iface.Spec.SubnetID != current.Spec.SubnetID {
		return nil, fmt.Errorf("%w: spec.vm_id/subnet_id cannot be changed", ErrValidation)
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

// firewallRulesEqual treats nil and empty-but-non-nil as equal (proto
// decoding of an empty repeated field can go either way depending on the
// call path) before falling back to reflect.DeepEqual, which does not.
func firewallRulesEqual(a, b []FirewallRule) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// UpdateFirewallRules replaces a NetworkInterface's ingress_rules/
// egress_rules wholesale (not a delta/merge, same convention as Resize's
// new_vcpu/new_memory_mb) and best-effort notifies the hypervisor currently
// running its VM via NATS (see publishUpdateACL) so the change actually
// takes effect on the host, not just in etcd.
func (s *Service) UpdateFirewallRules(ctx context.Context, tenantID, id string, ingress, egress []FirewallRule) (*NetworkInterface, error) {
	if err := validateFirewallRules(ingress); err != nil {
		return nil, err
	}
	if err := validateFirewallRules(egress); err != nil {
		return nil, err
	}

	n, err := s.interfaces.Get(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	old := n
	n.Spec.IngressRules = ingress
	n.Spec.EgressRules = egress
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

	s.publishUpdateACL(ctx, out)
	return &out, nil
}

// publishUpdateACL resolves the hypervisor currently running n's VM (via
// the same computeClient sweepOrphanedNetworkInterfaces already holds) and
// publishes an UpdateACLCommand to it. Best-effort only: a resolve failure,
// an unscheduled VM (empty hypervisor -- the eventual boot-time
// snap.Attach will carry the current rules anyway), a nil computeClient/
// js, or a publish failure all just log-and-return, never propagate to the
// caller -- the etcd write already succeeded, and this is host-state
// convergence, not correctness of the API call itself.
func (s *Service) publishUpdateACL(ctx context.Context, n NetworkInterface) {
	if n.Spec.VMID == "" || s.computeClient == nil || s.js == nil {
		return
	}
	vm, err := s.computeClient.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: n.Meta.TenantID, Id: n.Spec.VMID})
	if err != nil {
		slog.Warn("network: resolve hypervisor for update_acl failed, skipping notify", "netif_id", n.Meta.ID, "vm_id", n.Spec.VMID, "err", err)
		return
	}
	hypervisor := vm.GetStatus().GetHypervisor()
	if hypervisor == "" {
		return
	}

	var subnetCIDR, gatewayIP string
	var subnetLabels map[string]string
	if subnet, err := s.getSubnetForInterface(ctx, n.Meta.TenantID, n.Spec.SubnetID); err == nil {
		subnetCIDR = subnet.Spec.CIDR
		gatewayIP = subnet.Spec.GatewayIP
		subnetLabels = subnet.Meta.Labels
	}

	ingress, egress, err := s.EffectiveFirewallRules(ctx, &n)
	if err != nil {
		slog.Warn("network: resolve effective firewall rules for update_acl failed, skipping notify", "netif_id", n.Meta.ID, "err", err)
		return
	}

	cmd := UpdateACLCommand{
		IfaceID:         n.Meta.ID,
		VMID:            n.Spec.VMID,
		TenantID:        n.Meta.TenantID,
		SubnetID:        n.Spec.SubnetID,
		SubnetLabels:    subnetLabels,
		SubnetCIDR:      subnetCIDR,
		IPAddress:       n.Status.IPAddress,
		MACAddress:      n.Status.MACAddress,
		GatewayIP:       gatewayIP,
		IngressRules:    toFirewallRuleInfos(ingress),
		EgressRules:     toFirewallRuleInfos(egress),
		ResourceVersion: n.Meta.ResourceVersion,
	}
	payload, err := json.Marshal(cmd)
	if err != nil {
		slog.Error("network: marshal update_acl command failed", "netif_id", n.Meta.ID, "err", err)
		return
	}
	msg := nats.NewMsg(CmdSubjectUpdateACL(hypervisor))
	msg.Data = payload
	if _, err := s.js.PublishMsg(ctx, msg); err != nil {
		slog.Warn("network: publish update_acl failed", "netif_id", n.Meta.ID, "hypervisor", hypervisor, "err", err)
	}
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
