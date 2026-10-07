package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	networkv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/network/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
)

// ipreservationCmd manages IPReservations (see docs/specs/network.md
// 「IPReservation」).
func ipreservationCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet("ipreservation "+args[0], flag.ExitOnError)
	addr, token := commonFlags(fs)
	tenant := fs.String("tenant", "", "tenant ID (default: the token's tenant)")
	allTenants := fs.Bool("all-tenants", false, "list: every tenant (cross-tenant roles)")
	name := fs.String("name", "", "reservation name (create, idempotency key)")
	id := fs.String("id", "", "reservation ID (get/delete/add-finalizer/remove-finalizer)")
	networkID := fs.String("network", "", "Network ID; kyuusha picks a Subnet in -zone")
	zone := fs.String("zone", "", "availability zone (with -network)")
	subnetID := fs.String("subnet", "", "Subnet ID to reserve from (instead of -network/-zone)")
	address := fs.String("address", "", "a specific address to reserve (needs -subnet)")
	labels := fs.String("labels", "", "comma-separated key=value meta.labels")
	finalizer := fs.String("finalizer", "", "holder name (add-finalizer/remove-finalizer)")
	fs.Parse(args[1:])
	if *tenant == "" && !*allTenants {
		*tenant = resolveTenant(*token)
	}
	client := networkv1.NewIPReservationServiceClient(dialNetwork(*addr))
	ctx := authedContext(context.Background(), *token)
	switch args[0] {
	case "create":
		spec := &networkv1.IPReservationSpec{NetworkId: *networkID, Zone: *zone, SubnetId: *subnetID}
		if *address != "" {
			spec.RequestedAddresses = []string{*address}
		}
		r, err := client.Create(ctx, &networkv1.CreateIPReservationRequest{TenantId: *tenant, Name: *name, Spec: spec, Labels: parseKeyValues("-labels", *labels)})
		if err != nil {
			fatal("create: %v", err)
		}
		printIPReservation(r)
	case "get":
		r, err := client.Get(ctx, &networkv1.GetIPReservationRequest{TenantId: *tenant, Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		printIPReservation(r)
	case "list":
		resp, err := client.List(ctx, &networkv1.ListIPReservationsRequest{TenantId: *tenant})
		if err != nil {
			fatal("list: %v", err)
		}
		for _, r := range resp.GetItems() {
			printIPReservation(r)
		}
	case "delete":
		if _, err := client.Delete(ctx, &networkv1.DeleteIPReservationRequest{TenantId: *tenant, Id: *id}); err != nil {
			fatal("delete: %v", err)
		}
	case "add-finalizer", "remove-finalizer":
		if *finalizer == "" {
			fatal("-finalizer is required")
		}
		add := args[0] == "add-finalizer"
		r, err := client.Get(ctx, &networkv1.GetIPReservationRequest{TenantId: *tenant, Id: *id})
		if err != nil {
			fatal("get: %v", err)
		}
		var kept []*resourcev1.Finalizer
		present := false
		for _, f := range r.GetMeta().GetFinalizers() {
			if f.GetName() == *finalizer {
				present = true
				if !add {
					continue
				}
			}
			kept = append(kept, f)
		}
		if add == present {
			printIPReservation(r) // nothing to change
			return
		}
		if add {
			kept = append(kept, &resourcev1.Finalizer{Name: *finalizer})
		}
		r.Meta.Finalizers = kept
		if r, err = client.Update(ctx, &networkv1.UpdateIPReservationRequest{TenantId: *tenant, IpReservation: r}); err != nil {
			fatal("update: %v", err)
		}
		printIPReservation(r)
	default:
		usage()
		os.Exit(2)
	}
}

func printIPReservation(r *networkv1.IPReservation) {
	var fins []string
	for _, f := range r.GetMeta().GetFinalizers() {
		fins = append(fins, f.GetName())
	}
	fmt.Printf("id=%s name=%s tenant=%s labels=%s network=%s zone=%s subnet=%s phase=%s addresses=%s finalizers=%s deleted_at=%s rv=%d%s\n",
		r.GetMeta().GetId(), r.GetMeta().GetName(), r.GetMeta().GetTenantId(), formatKeyValues(r.GetMeta().GetLabels()),
		r.GetSpec().GetNetworkId(), firstNonEmpty(r.GetStatus().GetZone(), r.GetSpec().GetZone()), firstNonEmpty(r.GetStatus().GetSubnetId(), r.GetSpec().GetSubnetId()),
		r.GetStatus().GetPhase(), strings.Join(r.GetStatus().GetAddresses(), ","), strings.Join(fins, ","), deletedAtString(r.GetMeta().GetDeletedAt()),
		r.GetMeta().GetResourceVersion(), pendingReason(r.GetStatus().GetConditions()))
}
