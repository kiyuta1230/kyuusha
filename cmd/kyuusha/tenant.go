package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
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
	case "update":
		tenantUpdate(args[1:])
	case "delete":
		tenantDelete(args[1:])
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
	pciDeviceQuota := fs.String("pci-device-quota", "", "quota: comma-separated per-(vendor_id, device_id) PCI passthrough allotment, vendor_id:device_id:max_count (e.g. 10de:1c03:2) -- a pair absent here has an implicit max_count of 0, not unlimited; see docs/specs/quota.md")
	maxImages := fs.Int("max-images", 0, "quota: tenant-total Image count")
	maxSubnets := fs.Int("max-subnets", 0, "quota: tenant-total Subnet count")
	maxNetworkInterfaces := fs.Int("max-network-interfaces", 0, "quota: tenant-total NetworkInterface count")
	maxIPReservations := fs.Int("max-ip-reservations", 0, "quota: tenant-total IPReservation count")
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
				MaxVcpu:              int32(*maxVCPU),
				MaxMemoryMb:          *maxMemoryMB,
				MaxVolumeGb:          *maxVolumeGB,
				MaxVms:               int32(*maxVMs),
				MaxVcpuPerVm:         int32(*maxVCPUPerVM),
				MaxMemoryMbPerVm:     *maxMemoryMBPerVM,
				PciDevices:           parsePciDeviceQuota(*pciDeviceQuota),
				MaxImages:            int32(*maxImages),
				MaxSubnets:           int32(*maxSubnets),
				MaxNetworkInterfaces: int32(*maxNetworkInterfaces),
				MaxIpReservations:    int32(*maxIPReservations),
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

// tenantUpdate is a Get-then-merge-then-Update: only the flags actually
// passed on the command line (tracked via fs.Visit) overwrite the
// corresponding field, so omitting a quota flag leaves its current value
// untouched instead of resetting it to 0.
func tenantUpdate(args []string) {
	fs := flag.NewFlagSet("tenant update", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	id := fs.String("id", "", "tenant ID (required)")
	displayName := fs.String("display-name", "", "human-readable display name")
	maxVCPU := fs.Int("max-vcpu", 0, "quota: tenant-total vCPU")
	maxMemoryMB := fs.Int64("max-memory-mb", 0, "quota: tenant-total memory in MB")
	maxVolumeGB := fs.Int64("max-volume-gb", 0, "quota: tenant-total volume storage in GB")
	maxVMs := fs.Int("max-vms", 0, "quota: tenant-total VM count")
	maxVCPUPerVM := fs.Int("max-vcpu-per-vm", 0, "quota: per-VM vCPU cap")
	maxMemoryMBPerVM := fs.Int64("max-memory-mb-per-vm", 0, "quota: per-VM memory cap in MB")
	pciDeviceQuota := fs.String("pci-device-quota", "", "quota: comma-separated per-(vendor_id, device_id) PCI passthrough allotment, vendor_id:device_id:max_count (e.g. 10de:1c03:2) -- replaces the whole list, not merged with the existing one; see docs/specs/quota.md")
	maxImages := fs.Int("max-images", 0, "quota: tenant-total Image count")
	maxSubnets := fs.Int("max-subnets", 0, "quota: tenant-total Subnet count")
	maxNetworkInterfaces := fs.Int("max-network-interfaces", 0, "quota: tenant-total NetworkInterface count")
	maxIPReservations := fs.Int("max-ip-reservations", 0, "quota: tenant-total IPReservation count")
	fs.Parse(args)

	if *id == "" {
		fatal("-id is required")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	client := dialIdentity(*addr)
	ctx := authedContext(context.Background(), *token)

	tn, err := client.Get(ctx, &identityv1.GetTenantRequest{TenantId: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	if set["display-name"] {
		tn.Spec.DisplayName = *displayName
	}
	q := tn.Spec.Quota
	if set["max-vcpu"] {
		q.MaxVcpu = int32(*maxVCPU)
	}
	if set["max-memory-mb"] {
		q.MaxMemoryMb = *maxMemoryMB
	}
	if set["max-volume-gb"] {
		q.MaxVolumeGb = *maxVolumeGB
	}
	if set["max-vms"] {
		q.MaxVms = int32(*maxVMs)
	}
	if set["max-vcpu-per-vm"] {
		q.MaxVcpuPerVm = int32(*maxVCPUPerVM)
	}
	if set["max-memory-mb-per-vm"] {
		q.MaxMemoryMbPerVm = *maxMemoryMBPerVM
	}
	if set["pci-device-quota"] {
		q.PciDevices = parsePciDeviceQuota(*pciDeviceQuota)
	}
	if set["max-images"] {
		q.MaxImages = int32(*maxImages)
	}
	if set["max-subnets"] {
		q.MaxSubnets = int32(*maxSubnets)
	}
	if set["max-network-interfaces"] {
		q.MaxNetworkInterfaces = int32(*maxNetworkInterfaces)
	}
	if set["max-ip-reservations"] {
		q.MaxIpReservations = int32(*maxIPReservations)
	}

	updated, err := client.Update(ctx, &identityv1.UpdateTenantRequest{TenantId: *id, Tenant: tn})
	if err != nil {
		fatal("update: %v", err)
	}
	printTenant(updated)
}

func tenantDelete(args []string) {
	fs := flag.NewFlagSet("tenant delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	id := fs.String("id", "", "tenant ID (required)")
	fs.Parse(args)

	if *id == "" {
		fatal("-id is required")
	}
	client := dialIdentity(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &identityv1.DeleteTenantRequest{TenantId: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func printTenant(tn *identityv1.Tenant) {
	q := tn.GetSpec().GetQuota()
	fmt.Printf("id=%s name=%s display_name=%q phase=%s quota(vcpu=%d,memory_mb=%d,volume_gb=%d,vms=%d,vcpu/vm=%d,memory_mb/vm=%d,pci_devices=%s,images=%d,subnets=%d,network_interfaces=%d,ip_reservations=%d) rv=%d\n",
		tn.GetMeta().GetId(), tn.GetMeta().GetName(), tn.GetSpec().GetDisplayName(), tn.GetStatus().GetPhase(),
		q.GetMaxVcpu(), q.GetMaxMemoryMb(), q.GetMaxVolumeGb(), q.GetMaxVms(), q.GetMaxVcpuPerVm(), q.GetMaxMemoryMbPerVm(),
		formatPciDeviceQuota(q.GetPciDevices()),
		q.GetMaxImages(), q.GetMaxSubnets(), q.GetMaxNetworkInterfaces(), q.GetMaxIpReservations(),
		tn.GetMeta().GetResourceVersion())
}

// parsePciDeviceQuota turns -pci-device-quota's vendor_id:device_id:max_count
// entries into QuotaSpec.pci_devices -- mirroring vm create's -pci-devices
// parsing shape, but simpler: unlike a pci_address, neither vendor_id nor
// device_id ever contains a colon, so a plain 3-way split is safe here (see
// cmd/compute-agent/main.go's parsePciDevices for the case where it isn't).
func parsePciDeviceQuota(raw string) []*identityv1.PciDeviceQuota {
	var out []*identityv1.PciDeviceQuota
	for _, entry := range strings.Split(raw, ",") {
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			fatal("-pci-device-quota: malformed entry %q, want vendor_id:device_id:max_count", entry)
		}
		maxCount, err := strconv.Atoi(parts[2])
		if err != nil || maxCount < 0 {
			fatal("-pci-device-quota: malformed max_count in %q: %v", entry, err)
		}
		out = append(out, &identityv1.PciDeviceQuota{VendorId: parts[0], DeviceId: parts[1], MaxCount: int32(maxCount)})
	}
	return out
}

func formatPciDeviceQuota(quota []*identityv1.PciDeviceQuota) string {
	parts := make([]string, len(quota))
	for i, q := range quota {
		parts[i] = fmt.Sprintf("%s:%s:%d", q.GetVendorId(), q.GetDeviceId(), q.GetMaxCount())
	}
	return strings.Join(parts, ",")
}
