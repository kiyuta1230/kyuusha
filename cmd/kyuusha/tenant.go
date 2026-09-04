package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
)

func tenantCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		tenantCreate(args[1:])
	case "get":
		tenantGet(args[1:])
	case "list":
		tenantList(args[1:])
	case "watch":
		tenantWatch(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

// tenantCreate is admin-only (see internal/authz -- CreateTenantRequest
// carries no tenant_id, so only an admin-role token is authorized).
func tenantCreate(args []string) {
	fs := flag.NewFlagSet("tenant create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN); must carry role=admin")
	name := fs.String("name", "", "tenant name (idempotency key, required)")
	displayName := fs.String("display-name", "", "human-readable display name")
	maxVCPU := fs.Int("max-vcpu", 0, "quota: tenant-total vCPU")
	maxMemoryMB := fs.Int64("max-memory-mb", 0, "quota: tenant-total memory in MB")
	maxVolumeGB := fs.Int64("max-volume-gb", 0, "quota: tenant-total volume storage in GB")
	maxVMs := fs.Int("max-vms", 0, "quota: tenant-total VM count")
	maxVCPUPerVM := fs.Int("max-vcpu-per-vm", 0, "quota: per-VM vCPU cap")
	maxMemoryMBPerVM := fs.Int64("max-memory-mb-per-vm", 0, "quota: per-VM memory cap in MB")
	fs.Parse(args)

	if *name == "" {
		fatal("-name is required")
	}

	client := dialIdentity(*addr)
	ctx := authedContext(context.Background(), *token)

	tn, err := client.Create(ctx, &identityv1.CreateTenantRequest{
		Name: *name,
		Spec: &identityv1.TenantSpec{
			DisplayName: *displayName,
			Quota: &identityv1.QuotaSpec{
				MaxVcpu:          int32(*maxVCPU),
				MaxMemoryMb:      *maxMemoryMB,
				MaxVolumeGb:      *maxVolumeGB,
				MaxVms:           int32(*maxVMs),
				MaxVcpuPerVm:     int32(*maxVCPUPerVM),
				MaxMemoryMbPerVm: *maxMemoryMBPerVM,
			},
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printTenant(tn)
}

func tenantGet(args []string) {
	fs := flag.NewFlagSet("tenant get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	id := fs.String("id", "", "tenant ID (required)")
	fs.Parse(args)

	if *id == "" {
		fatal("-id is required")
	}
	client := dialIdentity(*addr)
	ctx := authedContext(context.Background(), *token)
	tn, err := client.Get(ctx, &identityv1.GetTenantRequest{TenantId: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printTenant(tn)
}

// tenantList with -id="" lists every tenant, which is admin-only in
// practice (see internal/authz): a non-admin's own tenant_id never matches
// an empty request.tenant_id, so only "list myself" (id = your own tenant)
// is authorized for a regular token.
func tenantList(args []string) {
	fs := flag.NewFlagSet("tenant list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	id := fs.String("id", "", "tenant ID to scope to (empty = all tenants, admin-only)")
	fs.Parse(args)

	client := dialIdentity(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &identityv1.ListTenantsRequest{TenantId: *id})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, tn := range resp.GetItems() {
		printTenant(tn)
	}
}

func tenantWatch(args []string) {
	fs := flag.NewFlagSet("tenant watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	id := fs.String("id", "", "tenant ID to scope to (empty = all tenants, admin-only)")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)

	client := dialIdentity(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &identityv1.WatchTenantsRequest{
		TenantId:             *id,
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
		if ev.GetType() == identityv1.TenantEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		tn := ev.GetTenant()
		fmt.Printf("%-10s %-24s phase=%-10s rv=%d\n",
			ev.GetType(), tn.GetMeta().GetId(), tn.GetStatus().GetPhase(), ev.GetResourceVersion())
	}
}

func printTenant(tn *identityv1.Tenant) {
	q := tn.GetSpec().GetQuota()
	fmt.Printf("id=%s name=%s display_name=%q phase=%s quota(vcpu=%d,memory_mb=%d,volume_gb=%d,vms=%d,vcpu/vm=%d,memory_mb/vm=%d) rv=%d\n",
		tn.GetMeta().GetId(), tn.GetMeta().GetName(), tn.GetSpec().GetDisplayName(), tn.GetStatus().GetPhase(),
		q.GetMaxVcpu(), q.GetMaxMemoryMb(), q.GetMaxVolumeGb(), q.GetMaxVms(), q.GetMaxVcpuPerVm(), q.GetMaxMemoryMbPerVm(),
		tn.GetMeta().GetResourceVersion())
}
