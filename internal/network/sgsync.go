package network

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
)

// SecurityGroup distribution (network-reconciler only; see
// docs/specs/snap.md「アドレス集合の配布」). Rules reach a host whole, with
// group/Network peers left as named address sets; the sets' members are
// kept current separately:
//
//   - a NetworkInterface gaining or losing an address, groups or its
//     Network's membership sends just that change (an Add/Remove SetUpdate)
//     to every Hypervisor whose interfaces' rules reference the set;
//   - an interface arriving on a Hypervisor (VM Running there) sends that
//     host a full copy of every set the interface's rules reference,
//     closing the gap between the snapshot compute took at boot and now;
//   - a group's rules changing re-sends update_acl (full policy) to every
//     interface it's attached to;
//   - every sgFullSyncInterval, each Hypervisor gets a full copy of every
//     set it references, read fresh from etcd -- the backstop that makes a
//     lost or reordered delta converge anyway.
//
// Versions are etcd revisions throughout, so deltas and full copies (from
// here or from SecurityPolicy's snapshot) are ordered on one clock.

// sgFullSyncInterval is the periodic full reconcile. Package var so tests
// can shrink it.
var sgFullSyncInterval = 30 * time.Second

var setUpdatesSent = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kyuusha_network_sg_set_updates_total",
	Help: "Address-set updates sent to Hypervisors by network-reconciler, by kind (delta: a membership change; full: an interface arriving on a host, or the periodic full reconcile).",
}, []string{"kind"})

type sgNIC struct {
	ip         string
	hypervisor string
	groups     []string
	sets       []string // memberSets
}

type sgSyncer struct {
	svc *Service

	mu     sync.Mutex
	nics   map[string]sgNIC
	groups map[string]SecurityGroup
	rev    int64 // last NetworkInterface event applied to nics
}

func newSGSyncer(svc *Service) *sgSyncer {
	return &sgSyncer{svc: svc, nics: map[string]sgNIC{}, groups: map[string]SecurityGroup{}}
}

func (y *sgSyncer) run(ctx context.Context) {
	go runWatchLoop(ctx, "security groups (sync)", func(ctx context.Context, rv int64) (<-chan SecurityGroupEvent, error) {
		return y.svc.WatchSecurityGroups(ctx, "", rv)
	}, ErrSecurityGroupHistoryPruned, func(context.Context) {
		y.mu.Lock()
		y.groups = map[string]SecurityGroup{}
		y.mu.Unlock()
	}, func(e SecurityGroupEvent) { y.handleGroup(ctx, e) })
	go runWatchLoop(ctx, "network interfaces (sg sync)", func(ctx context.Context, rv int64) (<-chan NetworkInterfaceEvent, error) {
		return y.svc.WatchNetworkInterfaces(ctx, "", rv)
	}, ErrNetworkInterfaceHistoryPruned, func(context.Context) {
		y.mu.Lock()
		y.nics = map[string]sgNIC{}
		y.mu.Unlock()
	}, func(e NetworkInterfaceEvent) { y.handleNIC(ctx, e) })

	t := time.NewTicker(sgFullSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			y.fullSync(ctx)
		}
	}
}

// groupSets is every set g's rules reference.
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

// hostsReferencing is every Hypervisor running an interface whose rules
// reference set. Caller holds y.mu.
func (y *sgSyncer) hostsReferencing(set string) []string {
	var out []string
	for _, n := range y.nics {
		if n.hypervisor == "" || slices.Contains(out, n.hypervisor) {
			continue
		}
		if slices.Contains(groupSetsOf(y.groups, n.groups), set) {
			out = append(out, n.hypervisor)
		}
	}
	return out
}

// membersOf is set's current members per the cache. Caller holds y.mu.
func (y *sgSyncer) membersOf(set string) []string {
	var out []string
	for _, n := range y.nics {
		if slices.Contains(n.sets, set) {
			out = append(out, n.ip)
		}
	}
	sort.Strings(out)
	return out
}

func (y *sgSyncer) handleNIC(ctx context.Context, e NetworkInterfaceEvent) {
	observed := time.Now()
	id := e.Object.Meta.ID
	y.mu.Lock()
	old := y.nics[id]
	var cur sgNIC
	if e.Type == EventDeleted {
		delete(y.nics, id)
	} else {
		cur = sgNIC{ip: e.Object.Status.IPAddress, hypervisor: e.Object.Status.Hypervisor, groups: e.Object.Spec.SecurityGroupIDs, sets: memberSets(e.Object)}
		y.nics[id] = cur
	}
	y.rev = e.ResourceVersion

	// Membership deltas: what left (or changed address), what joined.
	perHost := map[string][]SetUpdate{}
	addUpdate := func(set string, u SetUpdate) {
		for _, h := range y.hostsReferencing(set) {
			perHost[h] = append(perHost[h], u)
		}
	}
	for _, set := range old.sets {
		if old.ip != cur.ip || !slices.Contains(cur.sets, set) {
			addUpdate(set, SetUpdate{Name: set, Version: e.ResourceVersion, Remove: []string{old.ip}})
		}
	}
	for _, set := range cur.sets {
		if old.ip != cur.ip || !slices.Contains(old.sets, set) {
			addUpdate(set, SetUpdate{Name: set, Version: e.ResourceVersion, Add: []string{cur.ip}})
		}
	}
	// Arriving on a host (or referencing new groups there): give that host
	// full copies of everything the interface's rules need.
	var arrival []SetUpdate
	if cur.hypervisor != "" && (cur.hypervisor != old.hypervisor || !slices.Equal(cur.groups, old.groups)) {
		for _, set := range groupSetsOf(y.groups, cur.groups) {
			arrival = append(arrival, SetUpdate{Name: set, Version: e.ResourceVersion, Full: true, Members: y.membersOf(set)})
		}
		// The full copies already include this change.
		perHost[cur.hypervisor] = slices.DeleteFunc(perHost[cur.hypervisor], func(u SetUpdate) bool {
			return slices.ContainsFunc(arrival, func(a SetUpdate) bool { return a.Name == u.Name })
		})
	}
	y.mu.Unlock()

	for h, updates := range perHost {
		y.svc.publishUpdateSets(ctx, h, UpdateSetsCommand{Sets: updates, ObservedAt: observed}, "delta")
	}
	if len(arrival) > 0 {
		y.svc.publishUpdateSets(ctx, cur.hypervisor, UpdateSetsCommand{Sets: arrival, ObservedAt: observed}, "full")
	}
}

func (y *sgSyncer) handleGroup(ctx context.Context, e SecurityGroupEvent) {
	id := e.Object.Meta.ID
	y.mu.Lock()
	old, had := y.groups[id]
	if e.Type == EventDeleted {
		delete(y.groups, id)
	} else {
		y.groups[id] = e.Object
	}
	rulesChanged := e.Type == EventModified && had &&
		(!slices.Equal(old.Spec.IngressRules, e.Object.Spec.IngressRules) || !slices.Equal(old.Spec.EgressRules, e.Object.Spec.EgressRules))
	var attached []string
	if rulesChanged {
		for nid, n := range y.nics {
			if n.hypervisor != "" && slices.Contains(n.groups, id) {
				attached = append(attached, nid)
			}
		}
	}
	y.mu.Unlock()

	if len(attached) == 0 {
		return
	}
	ifaces, err := y.svc.interfaces.List(ctx, "")
	if err != nil {
		slog.Warn("network: list interfaces for a security group change failed", "security_group_id", id, "err", err)
		return
	}
	for _, n := range ifaces {
		if slices.Contains(attached, n.Meta.ID) {
			y.svc.publishUpdateACL(ctx, n, e.ResourceVersion)
		}
	}
	slog.Info("network: re-sent update_acl after a security group's rules changed", "security_group_id", id, "interfaces", len(attached))
}

// fullSync sends every Hypervisor a full copy of every set its interfaces'
// rules reference, read fresh from etcd.
func (y *sgSyncer) fullSync(ctx context.Context) {
	ifaces, rev, err := y.svc.interfaces.ListWithRevision(ctx, "")
	if err != nil {
		return
	}
	groupList, err := y.svc.secgroups.List(ctx, "")
	if err != nil {
		return
	}
	groups := make(map[string]SecurityGroup, len(groupList))
	for _, g := range groupList {
		groups[g.Meta.ID] = g
	}
	members := setMembers(ifaces)
	hostSets := map[string][]string{}
	for _, n := range ifaces {
		if n.Status.Hypervisor == "" {
			continue
		}
		for _, set := range groupSetsOf(groups, n.Spec.SecurityGroupIDs) {
			if !slices.Contains(hostSets[n.Status.Hypervisor], set) {
				hostSets[n.Status.Hypervisor] = append(hostSets[n.Status.Hypervisor], set)
			}
		}
	}
	now := time.Now()
	for h, sets := range hostSets {
		cmd := UpdateSetsCommand{ObservedAt: now}
		for _, set := range sets {
			cmd.Sets = append(cmd.Sets, SetUpdate{Name: set, Version: rev, Full: true, Members: members[set]})
		}
		y.svc.publishUpdateSets(ctx, h, cmd, "full")
	}
}

// publishUpdateSets is best-effort like publishUpdateACL: the periodic
// full reconcile repairs anything lost here.
func (s *Service) publishUpdateSets(ctx context.Context, hypervisor string, cmd UpdateSetsCommand, kind string) {
	if s.js == nil || len(cmd.Sets) == 0 {
		return
	}
	payload, err := json.Marshal(cmd)
	if err != nil {
		return
	}
	msg := nats.NewMsg(CmdSubjectUpdateSets(hypervisor))
	msg.Data = payload
	if _, err := s.js.PublishMsg(ctx, msg); err != nil {
		slog.Warn("network: publish update_sets failed", "hypervisor", hypervisor, "err", err)
		return
	}
	setUpdatesSent.WithLabelValues(kind).Add(float64(len(cmd.Sets)))
}
