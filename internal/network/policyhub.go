package network

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// PolicyHub distributes SecurityGroup policy to hypervisors, xDS-style
// (see docs/specs/snap.md「ポリシーの配布」and docs/architecture.md
// 「ポリシーの配布: xDSのようなgRPCストリーム」). Each compute-agent holds
// one stream (PolicyDistributionService.Stream) and subscribes to the
// NetworkInterfaces wired on it; the hub answers from a cache of every
// NetworkInterface and SecurityGroup, kept current by watching etcd:
//
//   - a newly subscribed interface gets its complete policy, plus full
//     copies of every address set its rules reference that the stream
//     hasn't been sent yet;
//   - a change to an interface (address, groups) or to one of its groups'
//     rules resends that interface's policy to the streams subscribed to
//     it;
//   - a change in a set's membership sends just the delta, to the streams
//     that hold a copy of the set;
//   - every resendInterval, and after the cache is rebuilt, every stream
//     gets everything again.
//
// The hub holds no state that matters beyond its own lifetime -- every
// replica of the network API runs its own, and an agent may connect to any
// of them. A stream whose buffer overflows is dropped; the agent
// reconnects and starts from full copies.
type PolicyHub struct {
	svc *Service

	mu      sync.Mutex
	ready   bool
	rev     int64 // newest etcd revision the cache reflects
	nics    map[string]NetworkInterface
	groups  map[string]SecurityGroup
	streams map[*PolicySubscription]bool
}

// PolicyMessage is one response's worth of changes for a stream.
type PolicyMessage struct {
	Revision   int64
	ObservedAt time.Time
	Interfaces []InterfacePolicy
	Sets       []SetUpdate
}

// InterfacePolicy is one interface's complete policy: its SecurityGroups'
// merged rules (Policy.Sets left empty -- set contents travel separately)
// and the address/context SNAP needs.
type InterfacePolicy struct {
	IfaceID      string
	VMID         string
	TenantID     string
	SubnetID     string
	SubnetLabels map[string]string
	SubnetCIDR   string
	GatewayIP    string
	IPAddress    string
	MACAddress   string
	Attach       AttachContext
	Policy       SecurityPolicy
	Version      int64
}

// SetUpdate changes one address set: Full replaces its members with
// Members; otherwise Add/Remove apply. Version is the etcd revision it
// reflects.
type SetUpdate struct {
	Name    string
	Version int64
	Full    bool
	Members []string
	Add     []string
	Remove  []string
}

// AttachContext is an interface's Network/NetworkClass context and its
// Subnet's allocated values, as VNAP/SNAP plugins receive it (see
// docs/specs/vnap.md).
type AttachContext struct {
	NetworkID              string
	NetworkLabels          map[string]string
	NetworkClass           string // the class's name
	NetworkClassAttributes map[string]string
	NetworkValues          map[string]int64
	NetworkAttributes      map[string]string
	SubnetValues           map[string]int64
	SubnetAttributes       map[string]string
	MTU                    int32
}

// policyResendInterval is the periodic full resend, the backstop for
// anything a bug might have let drift. Package var so tests can shrink it.
var policyResendInterval = 5 * time.Minute

// policyStreamBuffer bounds how far a stream may fall behind before it's
// dropped (and the agent starts over).
const policyStreamBuffer = 1024

var (
	policyStreamsConnected = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kyuusha_network_policy_streams",
		Help: "Policy distribution streams currently connected to this network replica, by hypervisor.",
	}, []string{"hypervisor"})
	policyRevision = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kyuusha_network_policy_revision",
		Help: "Newest etcd revision this replica's policy cache reflects.",
	})
	policyAckedRevision = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kyuusha_network_policy_acked_revision",
		Help: "Revision of the newest policy response a hypervisor acknowledged as applied (compare with kyuusha_network_policy_revision).",
	}, []string{"hypervisor"})
	policyNacks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kyuusha_network_policy_nacks_total",
		Help: "Policy responses a hypervisor reported it could not apply.",
	}, []string{"hypervisor"})
	setUpdatesSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kyuusha_network_sg_set_updates_total",
		Help: "Address-set updates sent to hypervisors, by kind (delta: a membership change; full: a set newly needed on a stream, or a resend).",
	}, []string{"kind"})
)

func NewPolicyHub(svc *Service) *PolicyHub {
	return &PolicyHub{svc: svc, nics: map[string]NetworkInterface{}, groups: map[string]SecurityGroup{}, streams: map[*PolicySubscription]bool{}}
}

// PolicySubscription is one connected stream.
type PolicySubscription struct {
	hub        *PolicyHub
	hypervisor string
	subscribed map[string]bool
	sentSets   map[string]bool
	out        chan PolicyMessage
	dropped    chan struct{}
	closed     bool
}

// Connect registers a stream for hypervisor. Nothing is sent until its
// first Subscribe.
func (h *PolicyHub) Connect(hypervisor string) *PolicySubscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &PolicySubscription{hub: h, hypervisor: hypervisor, subscribed: map[string]bool{}, sentSets: map[string]bool{},
		out: make(chan PolicyMessage, policyStreamBuffer), dropped: make(chan struct{})}
	h.streams[s] = true
	policyStreamsConnected.WithLabelValues(hypervisor).Inc()
	return s
}

// Messages is what to send on the stream, in order.
func (s *PolicySubscription) Messages() <-chan PolicyMessage { return s.out }

// Dropped is closed if the hub gave up on this stream (it fell too far
// behind); the caller should end the stream so the agent reconnects.
func (s *PolicySubscription) Dropped() <-chan struct{} { return s.dropped }

// Close unregisters the stream.
func (s *PolicySubscription) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remove(s)
}

// remove unregisters s. Caller holds h.mu.
func (h *PolicyHub) remove(s *PolicySubscription) {
	if s.closed {
		return
	}
	s.closed = true
	delete(h.streams, s)
	policyStreamsConnected.WithLabelValues(s.hypervisor).Dec()
}

// Ack records the agent's answer to a response with revision rev.
func (s *PolicySubscription) Ack(rev int64, errMsg string) {
	if errMsg != "" {
		policyNacks.WithLabelValues(s.hypervisor).Inc()
		slog.Warn("network: hypervisor could not apply policy", "hypervisor", s.hypervisor, "revision", rev, "err", errMsg)
		return
	}
	policyAckedRevision.WithLabelValues(s.hypervisor).Set(float64(rev))
}

// Subscribe replaces the set of interfaces s wants policy for, sending
// what's new.
func (s *PolicySubscription) Subscribe(ifaceIDs []string) {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	next := make(map[string]bool, len(ifaceIDs))
	var added []string
	for _, id := range ifaceIDs {
		next[id] = true
		if !s.subscribed[id] {
			added = append(added, id)
		}
	}
	s.subscribed = next
	if !h.ready {
		return // the cache's first load sends everything
	}
	msg := h.newMessage()
	for _, id := range added {
		if n, ok := h.nics[id]; ok {
			msg.Interfaces = append(msg.Interfaces, h.interfacePolicy(n))
		}
	}
	msg.Sets = h.syncSets(s, false)
	h.send(s, msg)
}

func (h *PolicyHub) newMessage() PolicyMessage {
	return PolicyMessage{Revision: h.rev, ObservedAt: time.Now()}
}

// send queues msg on s unless it's empty; a full buffer drops the stream.
// Caller holds h.mu.
func (h *PolicyHub) send(s *PolicySubscription, msg PolicyMessage) {
	if s.closed || len(msg.Interfaces)+len(msg.Sets) == 0 {
		return
	}
	for _, u := range msg.Sets {
		kind := "delta"
		if u.Full {
			kind = "full"
		}
		setUpdatesSent.WithLabelValues(kind).Inc()
	}
	select {
	case s.out <- msg:
	default:
		slog.Warn("network: policy stream fell too far behind, dropping it", "hypervisor", s.hypervisor)
		h.remove(s)
		close(s.dropped)
	}
}

// neededSets is every address set the rules of s's interfaces reference.
// Caller holds h.mu.
func (h *PolicyHub) neededSets(s *PolicySubscription) []string {
	var out []string
	for id := range s.subscribed {
		n, ok := h.nics[id]
		if !ok {
			continue
		}
		for _, set := range groupSetsOf(h.groups, n.Spec.SecurityGroupIDs) {
			if !slices.Contains(out, set) {
				out = append(out, set)
			}
		}
	}
	sort.Strings(out)
	return out
}

// syncSets returns full copies of the sets s now needs but hasn't been
// sent (all of them if all is set), and forgets those it no longer needs.
// Caller holds h.mu.
func (h *PolicyHub) syncSets(s *PolicySubscription, all bool) []SetUpdate {
	needed := h.neededSets(s)
	var out []SetUpdate
	for _, set := range needed {
		if all || !s.sentSets[set] {
			out = append(out, SetUpdate{Name: set, Version: h.rev, Full: true, Members: h.membersOf(set)})
		}
	}
	s.sentSets = make(map[string]bool, len(needed))
	for _, set := range needed {
		s.sentSets[set] = true
	}
	return out
}

func (h *PolicyHub) membersOf(set string) []string {
	var out []string
	for _, n := range h.nics {
		if slices.Contains(memberSets(n), set) {
			out = append(out, n.Status.IPAddress)
		}
	}
	sort.Strings(out)
	return out
}

// interfacePolicy builds n's complete policy from the cache (its Subnet
// and Network context are read from etcd). Caller holds h.mu.
func (h *PolicyHub) interfacePolicy(n NetworkInterface) InterfacePolicy {
	var groups []SecurityGroup
	for _, id := range n.Spec.SecurityGroupIDs {
		if g, ok := h.groups[id]; ok {
			groups = append(groups, g)
		}
	}
	ingress, egress, _ := mergeRules(groups)
	out := InterfacePolicy{
		IfaceID: n.Meta.ID, VMID: n.Spec.VMID, TenantID: n.Meta.TenantID, SubnetID: n.SubnetID(),
		IPAddress: n.Status.IPAddress, MACAddress: n.Status.MACAddress,
		Policy:  SecurityPolicy{SecurityGroupIDs: n.Spec.SecurityGroupIDs, IngressRules: ingress, EgressRules: egress},
		Version: h.rev,
	}
	if subnet, err := h.svc.getSubnetForInterface(context.Background(), n.Meta.TenantID, n.SubnetID()); err == nil {
		out.SubnetCIDR, out.GatewayIP = subnet.Status.IPv4()
		out.SubnetLabels = subnet.Meta.Labels
		out.Attach = h.svc.attachContext(context.Background(), subnet)
	}
	return out
}

// groupSetsOf is every set the rules of the groups ids reference.
func groupSetsOf(groups map[string]SecurityGroup, ids []string) []string {
	var out []string
	for _, id := range ids {
		g, ok := groups[id]
		if !ok {
			continue
		}
		for _, set := range g.referencedSets() {
			if !slices.Contains(out, set) {
				out = append(out, set)
			}
		}
	}
	return out
}

// Run keeps the cache current until ctx is done: list, then watch from
// the listed revision; on any watch failure, list again (and resend
// everything, since changes may have been missed).
func (h *PolicyHub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := h.runOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("network: policy cache watch ended, rebuilding", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchRetryDelay):
		}
	}
}

var errPolicyWatchClosed = errors.New("watch closed")

func (h *PolicyHub) runOnce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nics, nicRev, err := h.svc.interfaces.ListWithRevision(ctx, "")
	if err != nil {
		return err
	}
	groups, groupRev, err := h.svc.secgroups.ListWithRevision(ctx, "")
	if err != nil {
		return err
	}
	nicEvents, err := h.svc.interfaces.Watch(ctx, "", nicRev, nil)
	if err != nil {
		return err
	}
	groupEvents, err := h.svc.secgroups.Watch(ctx, "", groupRev, nil)
	if err != nil {
		return err
	}
	h.load(nics, groups, max(nicRev, groupRev))

	ticker := time.NewTicker(policyResendInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			h.resendAll()
		case e, ok := <-nicEvents:
			if !ok {
				return errPolicyWatchClosed
			}
			h.handleNIC(e)
		case e, ok := <-groupEvents:
			if !ok {
				return errPolicyWatchClosed
			}
			h.handleGroup(e)
		}
	}
}

func (h *PolicyHub) load(nics []NetworkInterface, groups []SecurityGroup, rev int64) {
	h.mu.Lock()
	h.nics = make(map[string]NetworkInterface, len(nics))
	for _, n := range nics {
		h.nics[n.Meta.ID] = n
	}
	h.groups = make(map[string]SecurityGroup, len(groups))
	for _, g := range groups {
		h.groups[g.Meta.ID] = g
	}
	h.rev = rev
	h.ready = true
	policyRevision.Set(float64(rev))
	h.mu.Unlock()
	h.resendAll()
}

// resendAll sends every stream its interfaces' policies and full copies
// of every set they need.
func (h *PolicyHub) resendAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.ready {
		return
	}
	for s := range h.streams {
		msg := h.newMessage()
		for id := range s.subscribed {
			if n, ok := h.nics[id]; ok {
				msg.Interfaces = append(msg.Interfaces, h.interfacePolicy(n))
			}
		}
		msg.Sets = h.syncSets(s, true)
		h.send(s, msg)
	}
}

func (h *PolicyHub) advance(rev int64) {
	if rev > h.rev {
		h.rev = rev
		policyRevision.Set(float64(rev))
	}
}

func (h *PolicyHub) handleNIC(e NetworkInterfaceEvent) {
	if e.Type == EventBookmark {
		h.mu.Lock()
		h.advance(e.ResourceVersion)
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance(e.ResourceVersion)
	id := e.Object.Meta.ID
	old, had := h.nics[id]
	var oldSets, newSets []string
	if had {
		oldSets = memberSets(old)
	}
	cur := e.Object
	if e.Type == EventDeleted {
		delete(h.nics, id)
	} else {
		h.nics[id] = cur
		newSets = memberSets(cur)
	}
	ipChanged := had && old.Status.IPAddress != cur.Status.IPAddress
	var deltas []SetUpdate
	for _, set := range oldSets {
		if e.Type == EventDeleted || ipChanged || !slices.Contains(newSets, set) {
			deltas = append(deltas, SetUpdate{Name: set, Version: e.ResourceVersion, Remove: []string{old.Status.IPAddress}})
		}
	}
	for _, set := range newSets {
		if ipChanged || !slices.Contains(oldSets, set) {
			deltas = append(deltas, SetUpdate{Name: set, Version: e.ResourceVersion, Add: []string{cur.Status.IPAddress}})
		}
	}
	policyChanged := e.Type != EventDeleted && (!had ||
		!slices.Equal(old.Spec.SecurityGroupIDs, cur.Spec.SecurityGroupIDs) ||
		old.Status.IPAddress != cur.Status.IPAddress || old.Status.MACAddress != cur.Status.MACAddress ||
		old.SubnetID() != cur.SubnetID())

	for s := range h.streams {
		msg := h.newMessage()
		subscribed := s.subscribed[id] && policyChanged
		if subscribed {
			msg.Interfaces = append(msg.Interfaces, h.interfacePolicy(cur))
			msg.Sets = h.syncSets(s, false) // its groups may now reference new sets
		}
		for _, d := range deltas {
			// A set just sent in full already includes this change.
			if s.sentSets[d.Name] && !slices.ContainsFunc(msg.Sets, func(u SetUpdate) bool { return u.Name == d.Name }) {
				msg.Sets = append(msg.Sets, d)
			}
		}
		h.send(s, msg)
	}
}

func (h *PolicyHub) handleGroup(e SecurityGroupEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance(e.ResourceVersion)
	if e.Type == EventBookmark {
		return
	}
	id := e.Object.Meta.ID
	old, had := h.groups[id]
	if e.Type == EventDeleted {
		delete(h.groups, id)
	} else {
		h.groups[id] = e.Object
	}
	if had && e.Type == EventModified &&
		slices.Equal(old.Spec.IngressRules, e.Object.Spec.IngressRules) && slices.Equal(old.Spec.EgressRules, e.Object.Spec.EgressRules) {
		return // only metadata/sharing changed: nothing a host enforces
	}
	for s := range h.streams {
		msg := h.newMessage()
		for nid := range s.subscribed {
			if n, ok := h.nics[nid]; ok && slices.Contains(n.Spec.SecurityGroupIDs, id) {
				msg.Interfaces = append(msg.Interfaces, h.interfacePolicy(n))
			}
		}
		if len(msg.Interfaces) > 0 {
			msg.Sets = h.syncSets(s, false)
		}
		h.send(s, msg)
	}
}
