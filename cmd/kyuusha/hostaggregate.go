package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

// hostaggregateCmd manages HostAggregates (admin-only; see
// docs/specs/vm-scheduling.md「HostAggregate」). update replaces whichever
// of -zone/-labels/-hypervisors is given, keeping the rest.
func hostaggregateCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("hostaggregate "+args[0], flag.ExitOnError)
	addr, token := commonFlags(fs)
	name := fs.String("name", "", "aggregate name (create)")
	id := fs.String("id", "", "aggregate ID (get/update/delete)")
	zone := fs.String("zone", "", "zone the members are in (create, required)")
	labels := fs.String("labels", "", "comma-separated key=value labels NetworkClass host_aggregate_selector matches")
	hypervisors := fs.String("hypervisors", "", "comma-separated member Hypervisor ids")
	fs.Parse(args[1:])
	client := computev1.NewHostAggregateServiceClient(dialNetwork(*addr))
	ctx := authedContext(context.Background(), *token)
	switch args[0] {
	case "create":
		a, err := client.Create(ctx, &computev1.CreateHostAggregateRequest{Name: *name, Spec: &computev1.HostAggregateSpec{
			Zone: *zone, Labels: parseKeyValues("-labels", *labels), Hypervisors: splitList(*hypervisors),
		}})
		if err != nil {
			fatal("create: %v", err)
		}
		printHostAggregate(a)
	case "get":
		a, err := client.Get(ctx, &computev1.GetHostAggregateRequest{Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		printHostAggregate(a)
	case "list":
		resp, err := client.List(ctx, &computev1.ListHostAggregatesRequest{})
		if err != nil {
			fatal("list: %v", err)
		}
		for _, a := range resp.GetItems() {
			printHostAggregate(a)
		}
	case "update":
		a, err := client.Get(ctx, &computev1.GetHostAggregateRequest{Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["zone"] {
			a.Spec.Zone = *zone
		}
		if set["labels"] {
			a.Spec.Labels = parseKeyValues("-labels", *labels)
		}
		if set["hypervisors"] {
			a.Spec.Hypervisors = splitList(*hypervisors)
		}
		if a, err = client.Update(ctx, &computev1.UpdateHostAggregateRequest{HostAggregate: a}); err != nil {
			fatal("update: %v", err)
		}
		printHostAggregate(a)
	case "delete":
		if _, err := client.Delete(ctx, &computev1.DeleteHostAggregateRequest{Id: *id}); err != nil {
			fatal("delete: %v", err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func printHostAggregate(a *computev1.HostAggregate) {
	fmt.Printf("id=%s name=%s zone=%s labels=%s hypervisors=%s rv=%d\n", a.GetMeta().GetId(), a.GetMeta().GetName(), a.GetSpec().GetZone(),
		formatKeyValues(a.GetSpec().GetLabels()), strings.Join(a.GetSpec().GetHypervisors(), ","), a.GetMeta().GetResourceVersion())
}
