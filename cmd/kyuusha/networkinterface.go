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
	case "set-security-groups":
		netifSetSecurityGroups(args[1:])
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
	labels := fs.String("labels", "", "comma-separated key=value meta.labels (see docs/specs/external-integration.md)")
	annotations := fs.String("annotations", "", "comma-separated key=value meta.annotations (values can't contain commas here; use the API for that)")
	vmID := fs.String("vm", "", "VM ID (required)")
	subnetID := fs.String("subnet", "", "subnet ID to pin to (or -network and -zone)")
	networkID := fs.String("network", "", "Network ID; kyuusha picks a Subnet in -zone")
	zone := fs.String("zone", "", "availability zone (with -network)")
	securityGroups := fs.String("security-groups", "", "comma-separated SecurityGroup IDs (default: the Network's default group)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *vmID == "" || (*subnetID == "" && (*networkID == "" || *zone == "")) {
		fatal("-tenant, -vm, and -subnet (or -network and -zone) are required")
	}

	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)

	n, err := client.Create(ctx, &networkv1.CreateNetworkInterfaceRequest{
		TenantId:    *tenant,
		Name:        *name,
		Labels:      parseKeyValues("-labels", *labels),
		Annotations: parseKeyValues("-annotations", *annotations),
		Spec: &networkv1.NetworkInterfaceSpec{
			VmId:      *vmID,
			SubnetId:  *subnetID,
			NetworkId: *networkID,
			Zone:      *zone,

			SecurityGroupIds: splitList(*securityGroups),
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
	tenant := fs.String("tenant", "", "tenant ID (required unless -all-tenants)")
	allTenants := fs.Bool("all-tenants", false, "every tenant at once (empty tenant_id); needs a cross-tenant role -- see docs/specs/authn-authz.md")
	fs.Parse(args)
	if !*allTenants {
		if *tenant == "" {
			*tenant = resolveTenant(*token)
		}
		if *tenant == "" {
			fatal("-tenant (or -all-tenants) is required")
		}
	} else if *tenant != "" {
		fatal("-tenant and -all-tenants are mutually exclusive")
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
	tenant := fs.String("tenant", "", "tenant ID (required unless -all-tenants)")
	allTenants := fs.Bool("all-tenants", false, "every tenant at once (empty tenant_id); needs a cross-tenant role -- see docs/specs/authn-authz.md")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)
	if !*allTenants {
		if *tenant == "" {
			*tenant = resolveTenant(*token)
		}
		if *tenant == "" {
			fatal("-tenant (or -all-tenants) is required")
		}
	} else if *tenant != "" {
		fatal("-tenant and -all-tenants are mutually exclusive")
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

// netifSetSecurityGroups calls SetSecurityGroups, replacing the attached
// groups wholesale; an empty -security-groups detaches all (deny all).
func netifSetSecurityGroups(args []string) {
	fs := flag.NewFlagSet("netif set-security-groups", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "network interface ID (required)")
	securityGroups := fs.String("security-groups", "", "comma-separated SecurityGroup IDs; empty detaches every group (deny all)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}
	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialNetworkInterfaces(*addr)
	ctx := authedContext(context.Background(), *token)
	n, err := client.SetSecurityGroups(ctx, &networkv1.SetSecurityGroupsRequest{TenantId: *tenant, Id: *id, SecurityGroupIds: splitList(*securityGroups)})
	if err != nil {
		fatal("set-security-groups: %v", err)
	}
	printNetworkInterface(n)
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
	fmt.Printf("id=%s name=%s tenant=%s labels=%s vm=%s network=%s zone=%s subnet=%s phase=%s hypervisor=%s ip=%s mac=%s security_groups=%s rv=%d\n",
		n.GetMeta().GetId(), n.GetMeta().GetName(), n.GetMeta().GetTenantId(), formatKeyValues(n.GetMeta().GetLabels()),
		n.GetSpec().GetVmId(), n.GetSpec().GetNetworkId(), n.GetSpec().GetZone(), firstNonEmpty(n.GetStatus().GetSubnetId(), n.GetSpec().GetSubnetId()),
		n.GetStatus().GetPhase(), n.GetStatus().GetHypervisor(), n.GetStatus().GetIpAddress(), n.GetStatus().GetMacAddress(),
		strings.Join(n.GetSpec().GetSecurityGroupIds(), ","),
		n.GetMeta().GetResourceVersion())
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
