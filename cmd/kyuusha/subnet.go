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

func dialSubnets(addr string) networkv1.SubnetServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return networkv1.NewSubnetServiceClient(conn)
}

func subnetCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		subnetCreate(args[1:])
	case "get":
		subnetGet(args[1:])
	case "list":
		subnetList(args[1:])
	case "watch":
		subnetWatch(args[1:])
	case "delete":
		subnetDelete(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func subnetCreate(args []string) {
	fs := flag.NewFlagSet("subnet create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "subnet name (idempotency key)")
	zone := fs.String("zone", "", "availability zone (required)")
	cidr := fs.String("cidr", "", "e.g. 10.0.1.0/24 (required)")
	gatewayIP := fs.String("gateway-ip", "", "gateway IP")
	dnsServers := fs.String("dns-servers", "", "comma-separated DNS server IPs")
	dnsSuffix := fs.String("dns-suffix", "", "DNS suffix; empty disables name resolution")
	meshGroup := fs.String("mesh-group", "", "declares intent to default-allow other Subnets sharing this value (same tenant only) -- enforced via NetworkInterface.status.effective_ingress_rules/effective_egress_rules, see docs/specs/network.md")
	allocatableIPRanges := fs.String("allocatable-ip-ranges", "", "comma-separated \"<start-ip>-<end-ip>\" ranges IPAM may draw from; empty means the whole cidr (minus network/broadcast/gateway-ip)")
	uniqueCidr := fs.Bool("unique-cidr", false, "reject this cidr if it overlaps any other Subnet (any tenant) that also has -unique-cidr set; use for Public IP address space")
	visibility := fs.String("visibility", "private", "private|public (default private) -- who besides the owning tenant may attach a NetworkInterface to this Subnet")
	sharedWithTenantIDs := fs.String("shared-with-tenant-ids", "", "comma-separated tenant IDs (besides the owner) allowed to attach a NetworkInterface to this Subnet (private only)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *zone == "" || *cidr == "" {
		fatal("-tenant, -zone, and -cidr are required")
	}

	client := dialSubnets(*addr)
	ctx := authedContext(context.Background(), *token)

	spec := &networkv1.SubnetSpec{
		Zone:       *zone,
		Cidr:       *cidr,
		GatewayIp:  *gatewayIP,
		DnsSuffix:  *dnsSuffix,
		MeshGroup:  *meshGroup,
		UniqueCidr: *uniqueCidr,
		Visibility: parseSubnetVisibility(*visibility),
	}
	if *dnsServers != "" {
		spec.DnsServers = strings.Split(*dnsServers, ",")
	}
	if *sharedWithTenantIDs != "" {
		spec.SharedWithTenantIds = strings.Split(*sharedWithTenantIDs, ",")
	}
	if *allocatableIPRanges != "" {
		spec.AllocatableIpRanges = strings.Split(*allocatableIPRanges, ",")
	}

	sn, err := client.Create(ctx, &networkv1.CreateSubnetRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec:     spec,
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printSubnet(sn)
}

func subnetGet(args []string) {
	fs := flag.NewFlagSet("subnet get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "subnet ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialSubnets(*addr)
	ctx := authedContext(context.Background(), *token)
	sn, err := client.Get(ctx, &networkv1.GetSubnetRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printSubnet(sn)
}

func subnetList(args []string) {
	fs := flag.NewFlagSet("subnet list", flag.ExitOnError)
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
	client := dialSubnets(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &networkv1.ListSubnetsRequest{TenantId: *tenant})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, sn := range resp.GetItems() {
		printSubnet(sn)
	}
}

func subnetWatch(args []string) {
	fs := flag.NewFlagSet("subnet watch", flag.ExitOnError)
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
	client := dialSubnets(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &networkv1.WatchSubnetsRequest{
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
		if ev.GetType() == networkv1.SubnetEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		sn := ev.GetSubnet()
		fmt.Printf("%-10s %-24s phase=%-10s vlan_id=%d rv=%d\n",
			ev.GetType(), sn.GetMeta().GetId(), sn.GetStatus().GetPhase(), sn.GetStatus().GetVlanId(), ev.GetResourceVersion())
	}
}

func subnetDelete(args []string) {
	fs := flag.NewFlagSet("subnet delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "subnet ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialSubnets(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &networkv1.DeleteSubnetRequest{TenantId: *tenant, Id: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func printSubnet(sn *networkv1.Subnet) {
	fmt.Printf("id=%s name=%s tenant=%s zone=%s cidr=%s mesh_group=%s unique_cidr=%t visibility=%s shared_with=%s phase=%s vlan_id=%d rv=%d\n",
		sn.GetMeta().GetId(), sn.GetMeta().GetName(), sn.GetMeta().GetTenantId(),
		sn.GetSpec().GetZone(), sn.GetSpec().GetCidr(), sn.GetSpec().GetMeshGroup(),
		sn.GetSpec().GetUniqueCidr(), sn.GetSpec().GetVisibility(),
		strings.Join(sn.GetSpec().GetSharedWithTenantIds(), ","),
		sn.GetStatus().GetPhase(), sn.GetStatus().GetVlanId(), sn.GetMeta().GetResourceVersion())
}

func parseSubnetVisibility(s string) networkv1.SubnetVisibility {
	switch s {
	case "private", "":
		return networkv1.SubnetVisibility_PRIVATE
	case "public":
		return networkv1.SubnetVisibility_PUBLIC
	default:
		fatal("-visibility must be private or public, got %q", s)
		return networkv1.SubnetVisibility_SUBNET_VISIBILITY_UNSPECIFIED
	}
}
