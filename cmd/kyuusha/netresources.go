package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
)

func dialNetwork(addr string) *grpc.ClientConn {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return conn
}

// specFlag reads a protojson spec from -spec (inline) or -spec-file.
func specFlag(inline, file string, into proto.Message) {
	raw := []byte(inline)
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			fatal("read -spec-file: %v", err)
		}
		raw = b
	}
	if len(raw) == 0 {
		fatal("-spec or -spec-file is required")
	}
	if err := protojson.Unmarshal(raw, into); err != nil {
		fatal("parse spec: %v", err)
	}
}

func commonFlags(fs *flag.FlagSet) (addr, token *string) {
	return fs.String("addr", "localhost:8080", "api-gateway address"), fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
}

// ---------------------------------------------------------------------------
// pool

func poolCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("pool "+args[0], flag.ExitOnError)
	addr, token := commonFlags(fs)
	name := fs.String("name", "", "pool name (create)")
	id := fs.String("id", "", "pool ID (get/delete)")
	spec := fs.String("spec", "", `AllocationPoolSpec as protojson, e.g. {"integer":{"ranges":[{"lo":100,"hi":199}]}}`)
	specFile := fs.String("spec-file", "", "file holding the protojson spec")
	labels := fs.String("labels", "", "comma-separated key=value meta.labels")
	fs.Parse(args[1:])
	client := networkv1.NewAllocationPoolServiceClient(dialNetwork(*addr))
	ctx := authedContext(context.Background(), *token)
	switch args[0] {
	case "create":
		s := &networkv1.AllocationPoolSpec{}
		specFlag(*spec, *specFile, s)
		p, err := client.Create(ctx, &networkv1.CreateAllocationPoolRequest{Name: *name, Spec: s, Labels: parseKeyValues("-labels", *labels)})
		if err != nil {
			fatal("create: %v", err)
		}
		printPool(p)
	case "get":
		p, err := client.Get(ctx, &networkv1.GetAllocationPoolRequest{Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		printPool(p)
	case "list":
		resp, err := client.List(ctx, &networkv1.ListAllocationPoolsRequest{})
		if err != nil {
			fatal("list: %v", err)
		}
		for _, p := range resp.GetItems() {
			printPool(p)
		}
	case "delete":
		if _, err := client.Delete(ctx, &networkv1.DeleteAllocationPoolRequest{Id: *id}); err != nil {
			fatal("delete: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func printPool(p *networkv1.AllocationPool) {
	spec, _ := protojson.Marshal(p.GetSpec())
	fmt.Printf("id=%s name=%s allocated=%d spec=%s rv=%d\n", p.GetMeta().GetId(), p.GetMeta().GetName(), p.GetStatus().GetAllocated(), spec, p.GetMeta().GetResourceVersion())
}

// ---------------------------------------------------------------------------
// netclass

func netclassCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("netclass "+args[0], flag.ExitOnError)
	addr, token := commonFlags(fs)
	tenant := fs.String("tenant", "", "get/list: only classes this tenant may use (default: the token's tenant; empty with a cross-tenant role: all)")
	name := fs.String("name", "", "class name (create)")
	id := fs.String("id", "", "class ID (get/delete)")
	spec := fs.String("spec", "", `NetworkClassSpec as protojson, e.g. {"subnet":{"*":{"refs":[{"poolId":"allocpool-..."}]}},"visibility":"VISIBILITY_PUBLIC"}`)
	specFile := fs.String("spec-file", "", "file holding the protojson spec")
	labels := fs.String("labels", "", "comma-separated key=value meta.labels")
	fs.Parse(args[1:])
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}
	client := networkv1.NewNetworkClassServiceClient(dialNetwork(*addr))
	ctx := authedContext(context.Background(), *token)
	switch args[0] {
	case "create":
		s := &networkv1.NetworkClassSpec{}
		specFlag(*spec, *specFile, s)
		c, err := client.Create(ctx, &networkv1.CreateNetworkClassRequest{Name: *name, Spec: s, Labels: parseKeyValues("-labels", *labels)})
		if err != nil {
			fatal("create: %v", err)
		}
		printClass(c)
	case "get":
		c, err := client.Get(ctx, &networkv1.GetNetworkClassRequest{TenantId: *tenant, Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		printClass(c)
	case "list":
		resp, err := client.List(ctx, &networkv1.ListNetworkClassesRequest{TenantId: *tenant})
		if err != nil {
			fatal("list: %v", err)
		}
		for _, c := range resp.GetItems() {
			printClass(c)
		}
	case "delete":
		if _, err := client.Delete(ctx, &networkv1.DeleteNetworkClassRequest{Id: *id}); err != nil {
			fatal("delete: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func printClass(c *networkv1.NetworkClass) {
	zones := make([]string, 0, len(c.GetSpec().GetSubnet()))
	for z := range c.GetSpec().GetSubnet() {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	fmt.Printf("id=%s name=%s visibility=%s zones=%s mtu=%d attributes=%s host_aggregate_selector=%s rv=%d\n", c.GetMeta().GetId(), c.GetMeta().GetName(),
		c.GetSpec().GetVisibility(), strings.Join(zones, ","), c.GetSpec().GetMtu(), formatKeyValues(c.GetSpec().GetAttributes()), formatKeyValues(c.GetSpec().GetHostAggregateSelector()), c.GetMeta().GetResourceVersion())
}

// ---------------------------------------------------------------------------
// network

func networkCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("network "+args[0], flag.ExitOnError)
	addr, token := commonFlags(fs)
	tenant := fs.String("tenant", "", "tenant ID (default: the token's tenant)")
	allTenants := fs.Bool("all-tenants", false, "list: every tenant (cross-tenant roles)")
	name := fs.String("name", "", "network name (create)")
	id := fs.String("id", "", "network ID (get/delete)")
	class := fs.String("class", "", "NetworkClass ID (create, required)")
	dnsSuffix := fs.String("dns-suffix", "", "DNS name space for every Subnet of the Network")
	visibility := fs.String("visibility", "private", "private|public (public needs a class that allows it)")
	sharedWith := fs.String("shared-with-tenant-ids", "", "comma-separated tenants that may attach NICs")
	labels := fs.String("labels", "", "comma-separated key=value meta.labels")
	fs.Parse(args[1:])
	if *tenant == "" && !*allTenants {
		*tenant = resolveTenant(*token)
	}
	client := networkv1.NewNetworkServiceClient(dialNetwork(*addr))
	ctx := authedContext(context.Background(), *token)
	switch args[0] {
	case "create":
		spec := &networkv1.NetworkSpec{NetworkClass: *class, DnsSuffix: *dnsSuffix, Visibility: networkv1.Visibility_VISIBILITY_PRIVATE}
		if *visibility == "public" {
			spec.Visibility = networkv1.Visibility_VISIBILITY_PUBLIC
		}
		if *sharedWith != "" {
			spec.SharedWithTenantIds = strings.Split(*sharedWith, ",")
		}
		n, err := client.Create(ctx, &networkv1.CreateNetworkRequest{TenantId: *tenant, Name: *name, Spec: spec, Labels: parseKeyValues("-labels", *labels)})
		if err != nil {
			fatal("create: %v", err)
		}
		printNetwork(n)
	case "get":
		n, err := client.Get(ctx, &networkv1.GetNetworkRequest{TenantId: *tenant, Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		printNetwork(n)
	case "list":
		resp, err := client.List(ctx, &networkv1.ListNetworksRequest{TenantId: *tenant})
		if err != nil {
			fatal("list: %v", err)
		}
		for _, n := range resp.GetItems() {
			printNetwork(n)
		}
	case "delete":
		if _, err := client.Delete(ctx, &networkv1.DeleteNetworkRequest{TenantId: *tenant, Id: *id}); err != nil {
			fatal("delete: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func printNetwork(n *networkv1.Network) {
	fmt.Printf("id=%s name=%s tenant=%s labels=%s class=%s visibility=%s phase=%s values=%s default_security_group=%s rv=%d%s\n",
		n.GetMeta().GetId(), n.GetMeta().GetName(), n.GetMeta().GetTenantId(), formatKeyValues(n.GetMeta().GetLabels()),
		n.GetSpec().GetNetworkClass(), n.GetSpec().GetVisibility(), n.GetStatus().GetPhase(), formatIntValues(n.GetStatus().GetValues()),
		n.GetStatus().GetDefaultSecurityGroupId(), n.GetMeta().GetResourceVersion(), pendingReason(n.GetStatus().GetConditions()))
}
