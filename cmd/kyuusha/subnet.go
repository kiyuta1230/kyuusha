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
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
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
	case "add-finalizer":
		subnetSetFinalizer(args[1:], true)
	case "remove-finalizer":
		subnetSetFinalizer(args[1:], false)
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
	labels := fs.String("labels", "", "comma-separated key=value meta.labels (see docs/specs/external-integration.md)")
	annotations := fs.String("annotations", "", "comma-separated key=value meta.annotations (values can't contain commas here; use the API for that)")
	network := fs.String("network", "", "Network ID this Subnet belongs to (required)")
	zone := fs.String("zone", "", "availability zone (required)")
	cidr := fs.String("cidr", "", "requested CIDR, only when the Network's class expects the requester to choose it (user-specified CIDR pool), e.g. 10.0.1.0/24")
	gatewayIP := fs.String("gateway-ip", "", "requested gateway for -cidr (default: per the class's gateway placement)")
	dnsServers := fs.String("dns-servers", "", "comma-separated DNS server IPs (default: the class's resolvers for this zone)")
	allocatableIPRanges := fs.String("allocatable-ip-ranges", "", "comma-separated \"<start-ip>-<end-ip>\" ranges IPAM may draw from; empty means the whole CIDR (minus network/broadcast/gateway)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *network == "" || *zone == "" {
		fatal("-tenant, -network, and -zone are required")
	}

	client := dialSubnets(*addr)
	ctx := authedContext(context.Background(), *token)

	spec := &networkv1.SubnetSpec{NetworkId: *network, Zone: *zone}
	if *cidr != "" {
		spec.RequestedAddresses = []*networkv1.SubnetAddress{{Cidr: *cidr, GatewayIp: *gatewayIP}}
	}
	if *dnsServers != "" {
		spec.DnsServers = strings.Split(*dnsServers, ",")
	}
	if *allocatableIPRanges != "" {
		spec.AllocatableIpRanges = strings.Split(*allocatableIPRanges, ",")
	}

	sn, err := client.Create(ctx, &networkv1.CreateSubnetRequest{
		TenantId:    *tenant,
		Name:        *name,
		Spec:        spec,
		Labels:      parseKeyValues("-labels", *labels),
		Annotations: parseKeyValues("-annotations", *annotations),
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
		fmt.Printf("%-10s %-24s phase=%-10s values=%s rv=%d\n",
			ev.GetType(), sn.GetMeta().GetId(), sn.GetStatus().GetPhase(), formatIntValues(sn.GetStatus().GetValues()), ev.GetResourceVersion())
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

// subnetSetFinalizer adds (add=true) or removes one finalizer via
// Get-then-Update, same as vm add-finalizer/remove-finalizer.
func subnetSetFinalizer(args []string, add bool) {
	verb := "remove-finalizer"
	if add {
		verb = "add-finalizer"
	}
	fs := flag.NewFlagSet("subnet "+verb, flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "subnet ID (required)")
	finalizer := fs.String("finalizer", "", "holder name, e.g. \"vpc.example.com/cleanup\" (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}
	if *tenant == "" || *id == "" || *finalizer == "" {
		fatal("-tenant, -id, and -finalizer are required")
	}
	client := dialSubnets(*addr)
	ctx := authedContext(context.Background(), *token)

	sn, err := client.Get(ctx, &networkv1.GetSubnetRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	var kept []*resourcev1.Finalizer
	present := false
	for _, f := range sn.GetMeta().GetFinalizers() {
		if f.GetName() == *finalizer {
			present = true
			if !add {
				continue
			}
		}
		kept = append(kept, f)
	}
	if add == present {
		printSubnet(sn) // nothing to change: idempotent no-op
		return
	}
	if add {
		kept = append(kept, &resourcev1.Finalizer{Name: *finalizer})
	}
	sn.Meta.Finalizers = kept
	updated, err := client.Update(ctx, &networkv1.UpdateSubnetRequest{TenantId: *tenant, Subnet: sn})
	if err != nil {
		fatal("update: %v", err)
	}
	printSubnet(updated)
}

func printSubnet(sn *networkv1.Subnet) {
	finalizerNames := make([]string, len(sn.GetMeta().GetFinalizers()))
	for i, f := range sn.GetMeta().GetFinalizers() {
		finalizerNames[i] = f.GetName()
	}
	var addrs []string
	for _, a := range sn.GetStatus().GetAddresses() {
		addrs = append(addrs, a.GetCidr()+"@"+a.GetGatewayIp())
	}
	fmt.Printf("id=%s name=%s tenant=%s labels=%s network=%s zone=%s phase=%s addresses=%s values=%s finalizers=%s deleted_at=%s rv=%d%s\n",
		sn.GetMeta().GetId(), sn.GetMeta().GetName(), sn.GetMeta().GetTenantId(), formatKeyValues(sn.GetMeta().GetLabels()),
		sn.GetSpec().GetNetworkId(), sn.GetSpec().GetZone(), sn.GetStatus().GetPhase(),
		strings.Join(addrs, ","), formatIntValues(sn.GetStatus().GetValues()),
		strings.Join(finalizerNames, ","), deletedAtString(sn.GetMeta().GetDeletedAt()), sn.GetMeta().GetResourceVersion(),
		pendingReason(sn.GetStatus().GetConditions()))
}
