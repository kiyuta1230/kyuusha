package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
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

// Service implements the SubnetService/NetworkInterfaceService CRUD+Watch
// surface against in-memory resource.Stores. Both resources' Create go
// straight to Ready with mocked allocation -- see mockNextVLANID/
// mockNextMAC below and docs/specs/network.md's "モックの範囲" section for
// exactly what's fake and what IPAM (a follow-up) will replace.
type Service struct {
	subnets    *resource.Store[Subnet, *Subnet]
	interfaces *resource.Store[NetworkInterface, *NetworkInterface]

	nextVLANID int32
	nextMACOct uint32
}

func NewService() *Service {
	return &Service{
		subnets: resource.NewStore[Subnet, *Subnet]("subnet", resource.StoreErrors{
			NotFound:      ErrSubnetNotFound,
			Conflict:      ErrSubnetConflict,
			HistoryPruned: ErrSubnetHistoryPruned,
		}),
		interfaces: resource.NewStore[NetworkInterface, *NetworkInterface]("netif", resource.StoreErrors{
			NotFound:      ErrNetworkInterfaceNotFound,
			Conflict:      ErrNetworkInterfaceConflict,
			HistoryPruned: ErrNetworkInterfaceHistoryPruned,
		}),
		nextVLANID: 100,
	}
}

// mockNextVLANID hands out a globally-incrementing placeholder VLAN ID,
// ignoring spec.zone entirely. Real IPAM allocates from a pool scoped per
// zone (docs/architecture.md: "VLAN IDプールはzoneごとに独立して持つ"), exclusively
// and synchronously against a real pool -- this is not that, just enough to
// make the API observable end to end.
func (s *Service) mockNextVLANID() int32 {
	return int32(atomic.AddInt32(&s.nextVLANID, 1))
}

// mockNextMAC hands out a placeholder locally-administered MAC address.
// Real IPAM would derive/allocate this alongside a real IP from the
// Subnet's CIDR; this doesn't even look at the Subnet.
func (s *Service) mockNextMAC() string {
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

	if existing, ok := s.subnets.LookupByName(tenantID, name); ok {
		return &existing, nil
	}

	out, err := s.subnets.Create(ctx, tenantID, name, Subnet{
		Spec:   spec,
		Status: SubnetStatus{Phase: SubnetPhaseReady, VLANID: s.mockNextVLANID()},
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
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

func (s *Service) DeleteSubnet(ctx context.Context, tenantID, id string) error {
	return s.subnets.Delete(ctx, tenantID, id)
}

func (s *Service) WatchSubnets(ctx context.Context, tenantID string, sinceRV int64) (<-chan SubnetEvent, error) {
	return s.subnets.Watch(ctx, tenantID, sinceRV)
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

	if existing, ok := s.interfaces.LookupByName(tenantID, name); ok {
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

	out, err := s.interfaces.Create(ctx, tenantID, name, NetworkInterface{
		Spec: spec,
		Status: NetworkInterfaceStatus{
			Phase:      NetworkInterfacePhaseReady,
			IPAddress:  "0.0.0.0", // mock: real IPAM allocates from subnet.Spec.CIDR (see mockNextVLANID's doc)
			MACAddress: s.mockNextMAC(),
		},
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
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

func (s *Service) DeleteNetworkInterface(ctx context.Context, tenantID, id string) error {
	return s.interfaces.Delete(ctx, tenantID, id)
}

func (s *Service) WatchNetworkInterfaces(ctx context.Context, tenantID string, sinceRV int64) (<-chan NetworkInterfaceEvent, error) {
	return s.interfaces.Watch(ctx, tenantID, sinceRV)
}
