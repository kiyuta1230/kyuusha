package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

// secgroupCmd manages SecurityGroups (see docs/specs/network.md
// 「SecurityGroup」). Rules are written protocol:port_range:peer, comma-
// separated: protocol is tcp/udp/icmp or empty (any), port_range empty for
// all ports, peer a CIDR, sg=<id> (sg=self for this group) or
// net=<network id> -- e.g. "tcp:22:0.0.0.0/0,::sg=self,icmp::net=network-1".
func secgroupCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("secgroup "+args[0], flag.ExitOnError)
	addr, token := commonFlags(fs)
	tenant := fs.String("tenant", "", "tenant ID (default: the token's tenant)")
	allTenants := fs.Bool("all-tenants", false, "list: every tenant (cross-tenant roles)")
	name := fs.String("name", "", "group name (create)")
	id := fs.String("id", "", "group ID (get/update/delete)")
	description := fs.String("description", "", "free-form description")
	ingress := fs.String("ingress", "", "rules allowing traffic into the VM, protocol:port_range:peer[,...]")
	egress := fs.String("egress", "", "rules allowing traffic out of the VM, protocol:port_range:peer[,...]")
	sharedWith := fs.String("shared-with-tenant-ids", "", "comma-separated tenants that may attach/reference the group")
	labels := fs.String("labels", "", "comma-separated key=value meta.labels")
	fs.Parse(args[1:])
	if *tenant == "" && !*allTenants {
		*tenant = resolveTenant(*token)
	}
	client := networkv1.NewSecurityGroupServiceClient(dialNetwork(*addr))
	ctx := authedContext(context.Background(), *token)
	switch args[0] {
	case "create":
		g, err := client.Create(ctx, &networkv1.CreateSecurityGroupRequest{TenantId: *tenant, Name: *name, Labels: parseKeyValues("-labels", *labels),
			Spec: &networkv1.SecurityGroupSpec{
				Description: *description, IngressRules: parseSGRules(*ingress), EgressRules: parseSGRules(*egress),
				SharedWithTenantIds: splitList(*sharedWith),
			}})
		if err != nil {
			fatal("create: %v", err)
		}
		printSecurityGroup(g)
	case "get":
		g, err := client.Get(ctx, &networkv1.GetSecurityGroupRequest{TenantId: *tenant, Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		printSecurityGroup(g)
	case "list":
		resp, err := client.List(ctx, &networkv1.ListSecurityGroupsRequest{TenantId: *tenant})
		if err != nil {
			fatal("list: %v", err)
		}
		for _, g := range resp.GetItems() {
			printSecurityGroup(g)
		}
	case "update":
		g, err := client.Get(ctx, &networkv1.GetSecurityGroupRequest{TenantId: *tenant, Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["description"] {
			g.Spec.Description = *description
		}
		if set["ingress"] {
			g.Spec.IngressRules = parseSGRules(*ingress)
		}
		if set["egress"] {
			g.Spec.EgressRules = parseSGRules(*egress)
		}
		if set["shared-with-tenant-ids"] {
			g.Spec.SharedWithTenantIds = splitList(*sharedWith)
		}
		if set["labels"] {
			g.Meta.Labels = parseKeyValues("-labels", *labels)
		}
		if g, err = client.Update(ctx, &networkv1.UpdateSecurityGroupRequest{TenantId: *tenant, SecurityGroup: g}); err != nil {
			fatal("update: %v", err)
		}
		printSecurityGroup(g)
	case "delete":
		if _, err := client.Delete(ctx, &networkv1.DeleteSecurityGroupRequest{TenantId: *tenant, Id: *id}); err != nil {
			fatal("delete: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func parseSGRules(s string) []*networkv1.SecurityGroupRule {
	var out []*networkv1.SecurityGroupRule
	for _, entry := range splitList(s) {
		parts := strings.SplitN(entry, ":", 3) // a CIDR peer may itself contain ':' (IPv6)
		if len(parts) != 3 {
			fatal("invalid rule %q: want protocol:port_range:peer", entry)
		}
		peer := &networkv1.SecurityGroupPeer{}
		switch {
		case strings.HasPrefix(parts[2], "sg="):
			peer.Peer = &networkv1.SecurityGroupPeer_SecurityGroupId{SecurityGroupId: strings.TrimPrefix(parts[2], "sg=")}
		case strings.HasPrefix(parts[2], "net="):
			peer.Peer = &networkv1.SecurityGroupPeer_NetworkId{NetworkId: strings.TrimPrefix(parts[2], "net=")}
		default:
			peer.Peer = &networkv1.SecurityGroupPeer_Cidr{Cidr: parts[2]}
		}
		out = append(out, &networkv1.SecurityGroupRule{Protocol: parts[0], PortRange: parts[1], Peer: peer})
	}
	return out
}

func formatSGRules(rules []*networkv1.SecurityGroupRule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		peer := r.GetPeer().GetCidr()
		switch {
		case r.GetPeer().GetSecurityGroupId() != "":
			peer = "sg=" + r.GetPeer().GetSecurityGroupId()
		case r.GetPeer().GetNetworkId() != "":
			peer = "net=" + r.GetPeer().GetNetworkId()
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%s", r.GetProtocol(), r.GetPortRange(), peer))
	}
	return strings.Join(parts, ",")
}

func printSecurityGroup(g *networkv1.SecurityGroup) {
	fmt.Printf("id=%s name=%s tenant=%s default_for=%s ingress=%s egress=%s shared_with=%s labels=%s rv=%d\n",
		g.GetMeta().GetId(), g.GetMeta().GetName(), g.GetMeta().GetTenantId(), g.GetStatus().GetDefaultForNetworkId(),
		formatSGRules(g.GetSpec().GetIngressRules()), formatSGRules(g.GetSpec().GetEgressRules()),
		strings.Join(g.GetSpec().GetSharedWithTenantIds(), ","), formatKeyValues(g.GetMeta().GetLabels()), g.GetMeta().GetResourceVersion())
}
