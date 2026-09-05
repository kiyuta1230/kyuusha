package network

import (
	"time"

	"gitlab.com/ki.yuta1230/kyuusha/internal/resource"
)

type FirewallRule struct {
	Protocol   string
	PortRange  string
	SourceCIDR string
	Action     string
}

type NetworkInterfaceSpec struct {
	VMID         string
	SubnetID     string
	IngressRules []FirewallRule
}

type NetworkInterfacePhase string

const (
	NetworkInterfacePhasePending   NetworkInterfacePhase = "Pending"
	NetworkInterfacePhaseBinding   NetworkInterfacePhase = "Binding"
	NetworkInterfacePhaseReady     NetworkInterfacePhase = "Ready"
	NetworkInterfacePhaseRebinding NetworkInterfacePhase = "Rebinding"
	NetworkInterfacePhaseDeleting  NetworkInterfacePhase = "Deleting"
	NetworkInterfacePhaseError     NetworkInterfacePhase = "Error"
)

type NetworkInterfaceStatus struct {
	Phase      NetworkInterfacePhase
	Conditions []resource.Condition
	IPAddress  string
	MACAddress string
	Hypervisor string
}

type NetworkInterface struct {
	Meta   resource.ObjectMeta
	Spec   NetworkInterfaceSpec
	Status NetworkInterfaceStatus
}

// Delegating methods so *NetworkInterface satisfies resource.Meta, letting
// it plug into the generic resource.Store.
func (n *NetworkInterface) GetID() string               { return n.Meta.ID }
func (n *NetworkInterface) SetID(id string)             { n.Meta.ID = id }
func (n *NetworkInterface) GetName() string             { return n.Meta.Name }
func (n *NetworkInterface) SetName(name string)         { n.Meta.Name = name }
func (n *NetworkInterface) GetTenantID() string         { return n.Meta.TenantID }
func (n *NetworkInterface) SetTenantID(id string)       { n.Meta.TenantID = id }
func (n *NetworkInterface) GetResourceVersion() int64   { return n.Meta.ResourceVersion }
func (n *NetworkInterface) SetResourceVersion(rv int64) { n.Meta.ResourceVersion = rv }
func (n *NetworkInterface) GetCreatedAt() time.Time     { return n.Meta.CreatedAt }
func (n *NetworkInterface) SetCreatedAt(t time.Time)    { n.Meta.CreatedAt = t }
