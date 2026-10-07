package computeagent

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/snap"
	"github.com/kiyuta1230/kyuusha/internal/compute-agent/vmm"

	networkagentv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/agent/v1"
	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// The policy stream (see docs/specs/snap.md「ポリシーの配布」): this host
// keeps one PolicyDistributionService.Stream open to the network service,
// subscribes to the NetworkInterfaces its drivers have wired, applies
// what comes back through SNAP, and acknowledges every response. Boot
// still wires each interface with the policy compute read at scheduling
// time, so a VM is right from its first packet; the stream keeps it right
// afterwards. On any stream failure it reconnects with jittered backoff
// and starts over from full copies.

// propagationSeconds is how long a change took from the network service
// noticing it to this host applying it -- the delay docs/specs/snap.md
// promises to keep to seconds.
var propagationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "kyuusha_compute_agent_sg_set_propagation_seconds",
	Help:    "Delay from the network service observing a SecurityGroup policy change to this host applying it, by kind (delta/full: address sets, policy: an interface's rules).",
	Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
}, []string{"kind"})

const (
	policyResubscribeInterval = time.Second
	policyBackoffMin          = time.Second
	policyBackoffMax          = 30 * time.Second
)

func (a *Agent) kickPolicyStream() {
	if a.policyKick == nil {
		return
	}
	select {
	case a.policyKick <- struct{}{}:
	default:
	}
}

func (a *Agent) runPolicyStream(ctx context.Context) {
	backoff := policyBackoffMin
	for ctx.Err() == nil {
		start := time.Now()
		err := a.policyStreamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > policyBackoffMax {
			backoff = policyBackoffMin // it was healthy for a while: not a crash loop
		}
		// Jitter, so a network replica's restart doesn't bring every
		// hypervisor back at the same instant.
		wait := backoff/2 + rand.N(backoff)
		slog.Warn("compute-agent: policy stream ended, reconnecting", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, policyBackoffMax)
	}
}

// wiredInterfaces is every NetworkInterface some driver currently has a
// tap for.
func (a *Agent) wiredInterfaces() []string {
	var ids []string
	for _, d := range a.Drivers {
		if d == nil {
			continue
		}
		for _, vm := range d.Running() {
			ids = append(ids, vm.NetworkInterfaces...)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (a *Agent) policyStreamOnce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := a.PolicyClient.Stream(ctx)
	if err != nil {
		return err
	}
	var sendMu sync.Mutex // Send isn't safe for concurrent use
	send := func(req *networkagentv1.PolicyStreamRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(req)
	}
	subscribe := func(ids []string) error {
		return send(&networkagentv1.PolicyStreamRequest{Msg: &networkagentv1.PolicyStreamRequest_Subscribe{
			Subscribe: &networkagentv1.Subscribe{Hypervisor: a.Hypervisor, InterfaceIds: ids},
		}})
	}
	current := a.wiredInterfaces()
	if err := subscribe(current); err != nil {
		return err
	}
	slog.Info("compute-agent: policy stream connected", "interfaces", len(current))

	// Keep the subscription in step with what's wired: right after a Boot
	// (kickPolicyStream), and on a short tick for everything else (stops,
	// deletes, adoption after a restart).
	subErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(policyResubscribeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-a.policyKick:
			}
			if next := a.wiredInterfaces(); !slices.Equal(next, current) {
				if err := subscribe(next); err != nil {
					subErr <- err
					return
				}
				current = next
			}
		}
	}()

	for {
		resp, err := stream.Recv()
		if err != nil {
			select {
			case serr := <-subErr:
				return errors.Join(err, serr)
			default:
				return err
			}
		}
		applyErr := a.applyPolicyResponse(resp)
		ack := &networkagentv1.Ack{Nonce: resp.GetNonce()}
		if applyErr != nil {
			ack.Error = applyErr.Error()
			slog.Error("compute-agent: apply policy failed", "revision", resp.GetRevision(), "err", applyErr)
		}
		if err := send(&networkagentv1.PolicyStreamRequest{Msg: &networkagentv1.PolicyStreamRequest_Ack{Ack: ack}}); err != nil {
			return err
		}
	}
}

// applyPolicyResponse applies the interfaces' policies first (which also
// creates any newly referenced set, empty), then the set updates (filling
// them).
func (a *Agent) applyPolicyResponse(resp *networkagentv1.PolicyStreamResponse) error {
	var errs []string
	observed := resp.GetObservedAt().AsTime()
	for _, p := range resp.GetInterfaces() {
		applied, err := a.applyInterfacePolicy(p)
		if err != nil {
			errs = append(errs, p.GetIfaceId()+": "+err.Error())
		} else if applied && resp.GetObservedAt() != nil {
			propagationSeconds.WithLabelValues("policy").Observe(time.Since(observed).Seconds())
		}
	}
	if sets := resp.GetSets(); len(sets) > 0 {
		updates := make([]vmm.SetUpdate, 0, len(sets))
		for _, u := range sets {
			updates = append(updates, vmm.SetUpdate{Name: u.GetName(), Version: u.GetVersion(), Full: u.GetFull(), Members: u.GetMembers(), Add: u.GetAdd(), Remove: u.GetRemove()})
		}
		applied, err := snap.UpdateSets(updates, a.SecurityBackendBin)
		if err != nil {
			errs = append(errs, "sets: "+err.Error())
		} else if resp.GetObservedAt() != nil {
			for _, u := range applied {
				kind := "delta"
				if u.Full {
					kind = "full"
				}
				propagationSeconds.WithLabelValues(kind).Observe(time.Since(observed).Seconds())
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// applyInterfacePolicy re-applies one interface's policy through whichever
// driver has its tap; applied=false if none does (no longer wired here)
// or a policy at least as new is already in effect.
func (a *Agent) applyInterfacePolicy(p *networkagentv1.InterfacePolicy) (bool, error) {
	if !a.shouldApplyACL(p.GetIfaceId(), p.GetVersion()) {
		return false, nil
	}
	at := p.GetAttach()
	update := vmm.ACLUpdate{
		Attach: vmm.AttachInfo{
			NetworkID: at.GetNetworkId(), NetworkLabels: at.GetNetworkLabels(), NetworkClass: at.GetNetworkClass(),
			NetworkClassAttributes: at.GetNetworkClassAttributes(), NetworkValues: at.GetNetworkValues(), NetworkAttributes: at.GetNetworkAttributes(),
			SubnetValues: at.GetSubnetValues(), SubnetAttributes: at.GetSubnetAttributes(), MTU: at.GetMtu(),
		},
		IfaceID: p.GetIfaceId(), SubnetID: p.GetSubnetId(), SubnetLabels: p.GetSubnetLabels(), SubnetCIDR: p.GetSubnetCidr(), GatewayIP: p.GetGatewayIp(),
		IPAddress: p.GetIpAddress(), MACAddress: p.GetMacAddress(),
		Policy: toVMMPolicy(p.GetPolicy()),
	}
	for _, driver := range a.Drivers {
		if driver == nil {
			continue
		}
		applied, err := driver.ApplyACL(p.GetVmId(), update)
		if !applied {
			continue
		}
		if err != nil {
			return false, err
		}
		a.recordAppliedACL(p.GetIfaceId(), p.GetVersion())
		return true, nil
	}
	return false, nil
}

func toVMMPolicy(p *networkv1.SecurityPolicy) vmm.SecurityPolicy {
	conv := func(rs []*networkv1.SecurityPolicyRule) []vmm.PolicyRule {
		var out []vmm.PolicyRule
		for _, r := range rs {
			out = append(out, vmm.PolicyRule{Protocol: r.GetProtocol(), PortRange: r.GetPortRange(), CIDR: r.GetCidr(), Set: r.GetSet()})
		}
		return out
	}
	out := vmm.SecurityPolicy{SecurityGroupIDs: p.GetSecurityGroupIds(), IngressRules: conv(p.GetIngressRules()), EgressRules: conv(p.GetEgressRules())}
	for _, a := range p.GetSets() {
		out.Sets = append(out.Sets, vmm.SetUpdate{Name: a.GetName(), Version: a.GetVersion(), Full: true, Members: a.GetMembers()})
	}
	return out
}

// shouldApplyACL: a policy at least as new as the last applied one (equal
// is a harmless resend of the same complete state).
func (a *Agent) shouldApplyACL(ifaceID string, version int64) bool {
	a.lastAppliedACLMu.Lock()
	defer a.lastAppliedACLMu.Unlock()
	return version >= a.lastAppliedACL[ifaceID]
}

func (a *Agent) recordAppliedACL(ifaceID string, version int64) {
	a.lastAppliedACLMu.Lock()
	defer a.lastAppliedACLMu.Unlock()
	if a.lastAppliedACL == nil {
		a.lastAppliedACL = make(map[string]int64)
	}
	a.lastAppliedACL[ifaceID] = version
}
