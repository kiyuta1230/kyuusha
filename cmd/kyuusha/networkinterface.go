package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func dialNetworkInterfaces(addr string) networkv1.NetworkInterfaceServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return networkv1.NewNetworkInterfaceServiceClient(conn)
}

func netifCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		netifCreate(args[1:])
	case "get":
		netifGet(args[1:])
	case "list":
		netifList(args[1:])
	case "watch":
		netifWatch(args[1:])
	case "set-firewall-rules":
		netifSetFirewallRules(args[1:])
	case "delete":
		netifDelete(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func netifCreate(args []string) {
	fs := flag.NewFlagSet("netif create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "network interface name (idempotency key)")
	vmID := fs.String("vm", "", "VM ID (required)")
	subnetID := fs.String("subnet", "", "subnet ID (required)")
	ingressRules := fs.String("ingress-rules", "", "comma-separated rules allowed into the VM, protocol:port_range:source_cidr:action (e.g. tcp:22:0.0.0.0/0:allow)")
	egressRules := fs.String("egress-rules", "", "comma-separated rules allowed out of the VM, same protocol:port_range:source_cidr:action shape")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *vmID == "" || *subnetID == "" {
		fatal("-tenant, -vm, and -subnet are required")
	}

	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)

	n, err := client.Create(ctx, &networkv1.CreateNetworkInterfaceRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec: &networkv1.NetworkInterfaceSpec{
			VmId:         *vmID,
			SubnetId:     *subnetID,
			IngressRules: parseFirewallRules(*ingressRules),
			EgressRules:  parseFirewallRules(*egressRules),
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printNetworkInterface(n)
}

func netifGet(args []string) {
	fs := flag.NewFlagSet("netif get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "network interface ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)
	n, err := client.Get(ctx, &networkv1.GetNetworkInterfaceRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printNetworkInterface(n)
}

func netifList(args []string) {
	fs := flag.NewFlagSet("netif list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &networkv1.ListNetworkInterfacesRequest{TenantId: *tenant})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, n := range resp.GetItems() {
		printNetworkInterface(n)
	}
}

func netifWatch(args []string) {
	fs := flag.NewFlagSet("netif watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &networkv1.WatchNetworkInterfacesRequest{
		TenantId:             *tenant,
		SinceResourceVersion: *since,
	})
	if err != nil {
		fatal("watch: %v", err)
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			fatal("watch: %v", err)
		}
		if ev.GetType() == networkv1.NetworkInterfaceEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		n := ev.GetNetworkInterface()
		fmt.Printf("%-10s %-24s phase=%-10s ip=%s rv=%d\n",
			ev.GetType(), n.GetMeta().GetId(), n.GetStatus().GetPhase(), n.GetStatus().GetIpAddress(), ev.GetResourceVersion())
	}
}

// netifSetFirewallRules calls UpdateFirewallRules, replacing ingress_rules/
// egress_rules wholesale (not a merge -- see the RPC's own doc comment): a
// caller who wants to keep one direction unchanged must pass its current
// value again, e.g. by first `netif get`-ting the existing rules.
func netifSetFirewallRules(args []string) {
	fs := flag.NewFlagSet("netif set-firewall-rules", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "network interface ID (required)")
	ingressRules := fs.String("ingress-rules", "", "comma-separated rules allowed into the VM, protocol:port_range:source_cidr:action (e.g. tcp:22:0.0.0.0/0:allow); empty clears ingress_rules entirely")
	egressRules := fs.String("egress-rules", "", "comma-separated rules allowed out of the VM, same protocol:port_range:source_cidr:action shape; empty clears egress_rules entirely")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}
	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}

	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)
	n, err := client.UpdateFirewallRules(ctx, &networkv1.UpdateFirewallRulesRequest{
		TenantId:     *tenant,
		Id:           *id,
		IngressRules: parseFirewallRules(*ingressRules),
		EgressRules:  parseFirewallRules(*egressRules),
	})
	if err != nil {
		fatal("set-firewall-rules: %v", err)
	}
	printNetworkInterface(n)
}

func parseFirewallRules(s string) []*networkv1.FirewallRule {
	if s == "" {
		return nil
	}
	var out []*networkv1.FirewallRule
	for _, entry := range strings.Split(s, ",") {
		parts := strings.SplitN(entry, ":", 4)
		if len(parts) != 4 {
			fatal("invalid firewall rule %q: want protocol:port_range:source_cidr:action", entry)
		}
		out = append(out, &networkv1.FirewallRule{
			Protocol: parts[0], PortRange: parts[1], SourceCidr: parts[2], Action: parts[3],
		})
	}
	return out
}

func netifDelete(args []string) {
	fs := flag.NewFlagSet("netif delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "network interface ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &networkv1.DeleteNetworkInterfaceRequest{TenantId: *tenant, Id: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func printNetworkInterface(n *networkv1.NetworkInterface) {
	fmt.Printf("id=%s name=%s tenant=%s vm=%s subnet=%s phase=%s ip=%s mac=%s ingress_rules=%s egress_rules=%s rv=%d\n",
		n.GetMeta().GetId(), n.GetMeta().GetName(), n.GetMeta().GetTenantId(),
		n.GetSpec().GetVmId(), n.GetSpec().GetSubnetId(),
		n.GetStatus().GetPhase(), n.GetStatus().GetIpAddress(), n.GetStatus().GetMacAddress(),
		formatFirewallRules(n.GetSpec().GetIngressRules()), formatFirewallRules(n.GetSpec().GetEgressRules()),
		n.GetMeta().GetResourceVersion())
}

func formatFirewallRules(rules []*networkv1.FirewallRule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, fmt.Sprintf("%s:%s:%s:%s", r.GetProtocol(), r.GetPortRange(), r.GetSourceCidr(), r.GetAction()))
	}
	return strings.Join(parts, ",")
}
