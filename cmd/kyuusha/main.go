// Command kyuusha is the CLI client for the kyuusha services. It is a thin
// wrapper over the gRPC API, talking to api-gateway (not backend services
// directly); see docs/architecture.md "API消費者の多様化に備える" and
// "認証・認可とHypervisor登録".
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	computev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/identity/v1"
	resourcev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/resource/v1"
	"github.com/kiyuta1230/kyuusha/internal/authn"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "vm":
		vmCmd(os.Args[2:])
	case "tenant":
		tenantCmd(os.Args[2:])
	case "hypervisor":
		hypervisorCmd(os.Args[2:])
	case "hostaggregate":
		hostaggregateCmd(os.Args[2:])
	case "image":
		imageCmd(os.Args[2:])
	case "subnet":
		subnetCmd(os.Args[2:])
	case "pool":
		poolCmd(os.Args[2:])
	case "netclass":
		netclassCmd(os.Args[2:])
	case "network":
		networkCmd(os.Args[2:])
	case "netif":
		netifCmd(os.Args[2:])
	case "volume":
		volumeCmd(os.Args[2:])
	case "volattach":
		volattachCmd(os.Args[2:])
	case "storageconn":
		storageConnCmd(os.Args[2:])
	case "token":
		tokenCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  kyuusha vm <create|get|list|watch|console|delete|stop|start|resize|migrate|attach-volume|detach-volume|reboot|hard-reboot|add-finalizer|remove-finalizer> [flags]
  kyuusha tenant <create|get|list|watch|update|delete> [flags]
  kyuusha hypervisor <get|list|watch|set-schedulable> [flags]   (admin-only)
  kyuusha hostaggregate <create|get|list|update|delete> [flags]   (admin-only)
  kyuusha hypervisor bootstrap-token create -zone=... [flags]   (dev-only, local signing; see internal/bootstraptoken)
  kyuusha image <create|build|get|list|watch|share|delete> [flags]
  kyuusha image build -dockerfile=... -context=... -registry=... -repo=... -kernel-url=... [flags]   (builds a KERNEL_ROOTFS Image from a Dockerfile's rootfs; requires docker/tar/mkfs.ext4 locally, see docs/specs/image.md)
  kyuusha pool <create|get|list|delete> [flags]   (admin-only; -spec is protojson AllocationPoolSpec)
  kyuusha netclass <create|get|list|delete> [flags]   (create/delete admin-only; -spec is protojson NetworkClassSpec)
  kyuusha network <create|get|list|delete> [flags]
  kyuusha subnet <create|get|list|watch|delete|add-finalizer|remove-finalizer> [flags]
  kyuusha netif <create|get|list|watch|set-firewall-rules|delete> [flags]
  kyuusha volume <create|get|list|watch|delete> [flags]
  kyuusha volattach <create|get|list|watch|delete> [flags]
  kyuusha storageconn <create|get|list|watch|delete> [flags]   (admin-only)
  kyuusha token mint [flags]   (dev-only; see hack/devkeys/README.md)`)
}

func vmCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		vmCreate(args[1:])
	case "get":
		vmGet(args[1:])
	case "list":
		vmList(args[1:])
	case "watch":
		vmWatch(args[1:])
	case "console":
		vmConsole(args[1:])
	case "delete":
		vmDelete(args[1:])
	case "stop":
		vmStop(args[1:])
	case "start":
		vmStart(args[1:])
	case "resize":
		vmResize(args[1:])
	case "migrate":
		vmMigrate(args[1:])
	case "attach-volume":
		vmAttachVolume(args[1:])
	case "detach-volume":
		vmDetachVolume(args[1:])
	case "reboot":
		vmReboot(args[1:], false)
	case "hard-reboot":
		vmReboot(args[1:], true)
	case "add-finalizer":
		vmAddFinalizer(args[1:])
	case "remove-finalizer":
		vmRemoveFinalizer(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func dial(addr string) computev1.VirtualMachineServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return computev1.NewVirtualMachineServiceClient(conn)
}

func dialHypervisors(addr string) computev1.HypervisorServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return computev1.NewHypervisorServiceClient(conn)
}

func dialIdentity(addr string) identityv1.TenantServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return identityv1.NewTenantServiceClient(conn)
}

// authedContext attaches the bearer token api-gateway expects. token
// defaults to $KYUUSHA_TOKEN so scripts don't have to pass -token
// everywhere; see `kyuusha token mint` for how to get one in dev.
func authedContext(ctx context.Context, token string) context.Context {
	if token == "" {
		token = os.Getenv("KYUUSHA_TOKEN")
	}
	if token == "" {
		fatal("no token: pass -token or set $KYUUSHA_TOKEN (see 'kyuusha token mint')")
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// resolveTenant is the CLI-only convenience `docs/open-questions.md`'s "CLIの
// -tenantフラグをトークンのクレームからデフォルトすべきか" describes: if
// -tenant was left unset, decode token (or $KYUUSHA_TOKEN)'s tenant_id claim
// without verifying its signature (authz still fully verifies+enforces it
// server-side; this is purely a client-side UX shortcut) and use that as the
// default. Deliberately returns "" -- leaving the caller's existing
// "-tenant is required" fatal to fire -- whenever the token carries a
// cross-tenant Role (admin/storage-admin/network-admin/viewer): for those,
// tenant_id is wherever the token happened to be minted for, not "the tenant
// to operate on", so auto-defaulting would silently pick the wrong tenant
// rather than the caller's actual intent. Never touches wire protocol or the
// server's authz model.
func resolveTenant(token string) string {
	if token == "" {
		token = os.Getenv("KYUUSHA_TOKEN")
	}
	if token == "" {
		return ""
	}
	claims := &authn.Claims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return ""
	}
	if claims.Role != "" {
		return ""
	}
	return claims.TenantID
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func vmCreate(args []string) {
	fs := flag.NewFlagSet("vm create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "VM name (idempotency key)")
	labels := fs.String("labels", "", "comma-separated key=value meta.labels (see docs/specs/external-integration.md)")
	annotations := fs.String("annotations", "", "comma-separated key=value meta.annotations (values can't contain commas here; use the API for that)")
	image := fs.String("image", "", "image ID (required)")
	vcpu := fs.Int("vcpu", 1, "vCPU count")
	memoryMB := fs.Int64("memory-mb", 1024, "memory in MB")
	driverHint := fs.String("driver-hint", "", "VMM driver: firecracker|cloud-hypervisor (empty: server default, FIRECRACKER). Must match the Image's format -- KERNEL_ROOTFS accepts either, QCOW2 requires cloud-hypervisor; see docs/specs/image.md")
	networks := fs.String("networks", "", "comma-separated Network IDs, one NIC each (first one is primary); kyuusha picks each NIC's Subnet in -zone")
	subnets := fs.String("subnets", "", "comma-separated Subnet IDs to pin NICs to instead (after any -networks NICs); all must be in the VM's zone")
	zone := fs.String("zone", "", "availability zone (required with -networks; otherwise taken from -subnets)")
	volumes := fs.String("volumes", "", "comma-separated Volume IDs to attach at boot (see docs/specs/volume.md; attach-before-boot only -- a Volume added after the VM is already Running is not attached)")
	pciDevices := fs.String("pci-devices", "", "comma-separated PCI passthrough requests, vendor_id:device_id[:count] (count defaults to 1, e.g. 10de:1c03 or 10de:1c03:2) -- requires -driver-hint=cloud-hypervisor; see docs/specs/virtual-machine.md \"PCIデバイスパススルー\"")
	numaPinned := fs.Bool("numa-pinned", false, "pin this VM's vCPUs/memory to a single host NUMA node the scheduler picks (see docs/architecture.md's NUMA/CPUピニング section); rejected if no Hypervisor has a node with enough spare vcpu/memory_mb")
	userDataFile := fs.String("user-data-file", "", "path to a cloud-init user-data file (NoCloud seed disk; see docs/architecture.md \"UserData注入\"); empty means don't inject anything")
	wait := fs.Bool("wait", false, "block until the VM reaches Running or Error")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *image == "" {
		fatal("-tenant and -image are required")
	}

	var netifs []*computev1.NetworkAttachment
	for _, networkID := range strings.Split(*networks, ",") {
		if networkID != "" {
			netifs = append(netifs, &computev1.NetworkAttachment{NetworkId: networkID, Primary: len(netifs) == 0})
		}
	}
	for _, subnetID := range strings.Split(*subnets, ",") {
		if subnetID != "" {
			netifs = append(netifs, &computev1.NetworkAttachment{SubnetId: subnetID, Primary: len(netifs) == 0})
		}
	}

	var volRequests []*computev1.VolumeRequest
	for _, volumeID := range strings.Split(*volumes, ",") {
		if volumeID == "" {
			continue
		}
		volRequests = append(volRequests, &computev1.VolumeRequest{VolumeId: volumeID})
	}

	var pciDeviceRequests []*computev1.PciDeviceRequest
	for _, entry := range strings.Split(*pciDevices, ",") {
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 {
			fatal("-pci-devices: malformed entry %q, want vendor_id:device_id[:count]", entry)
		}
		count := int32(1)
		if len(parts) == 3 {
			n, err := strconv.Atoi(parts[2])
			if err != nil || n <= 0 {
				fatal("-pci-devices: malformed count in %q: %v", entry, err)
			}
			count = int32(n)
		}
		pciDeviceRequests = append(pciDeviceRequests, &computev1.PciDeviceRequest{VendorId: parts[0], DeviceId: parts[1], Count: count})
	}

	var userData string
	if *userDataFile != "" {
		b, err := os.ReadFile(*userDataFile)
		if err != nil {
			fatal("-user-data-file: %v", err)
		}
		userData = string(b)
	}

	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)

	vm, err := client.Create(ctx, &computev1.CreateVirtualMachineRequest{
		TenantId:    *tenant,
		Name:        *name,
		Labels:      parseKeyValues("-labels", *labels),
		Annotations: parseKeyValues("-annotations", *annotations),
		Spec: &computev1.VirtualMachineSpec{
			ImageId:           *image,
			Vcpu:              int32(*vcpu),
			MemoryMb:          *memoryMB,
			DriverHint:        parseVmmDriver(*driverHint),
			NetworkInterfaces: netifs,
			Zone:              *zone,
			Volumes:           volRequests,
			PciDevices:        pciDeviceRequests,
			NumaPinned:        *numaPinned,
			UserData:          userData,
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printVM(vm)

	if !*wait {
		return
	}
	waitForTerminal(ctx, client, *tenant, vm)
}

// waitForTerminal implements the "Create -> Watch until done" client-side
// pattern from docs/architecture.md instead of polling.
func waitForTerminal(ctx context.Context, client computev1.VirtualMachineServiceClient, tenant string, vm *computev1.VirtualMachine) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stream, err := client.Watch(ctx, &computev1.WatchVirtualMachinesRequest{
		TenantId:             tenant,
		SinceResourceVersion: vm.GetMeta().GetResourceVersion() - 1,
	})
	if err != nil {
		fatal("watch: %v", err)
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			fatal("watch closed before reaching a terminal phase")
		}
		if err != nil {
			fatal("watch: %v", err)
		}
		if ev.GetType() == computev1.VirtualMachineEvent_BOOKMARK || ev.GetVm().GetMeta().GetId() != vm.GetMeta().GetId() {
			continue
		}
		got := ev.GetVm()
		fmt.Fprintf(os.Stderr, "... phase=%s\n", got.GetStatus().GetPhase())
		switch got.GetStatus().GetPhase() {
		case "Running", "Error":
			printVM(got)
			return
		}
	}
}

func vmGet(args []string) {
	fs := flag.NewFlagSet("vm get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	vm, err := client.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printVM(vm)
}

func vmDelete(args []string) {
	fs := flag.NewFlagSet("vm delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &computev1.DeleteVirtualMachineRequest{TenantId: *tenant, Id: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func vmStop(args []string) {
	fs := flag.NewFlagSet("vm stop", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	force := fs.Bool("force", false, "SIGKILL immediately instead of SIGTERM-then-grace-period-then-SIGKILL")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	vm, err := client.Stop(ctx, &computev1.StopVirtualMachineRequest{TenantId: *tenant, Id: *id, Force: *force})
	if err != nil {
		fatal("stop: %v", err)
	}
	printVM(vm)
}

func vmStart(args []string) {
	fs := flag.NewFlagSet("vm start", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	vm, err := client.Start(ctx, &computev1.StartVirtualMachineRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("start: %v", err)
	}
	printVM(vm)
}

// vmResize changes vcpu/memory_mb of a VM -- cold (Stopped) or live
// (Running+CLOUD_HYPERVISOR, no downtime), server-side branching (see
// docs/specs/virtual-machine.md). No resource_version flag: like vmStop/
// vmStart, Resize does its own Get-then-mutate-then-Update server-side.
func vmResize(args []string) {
	fs := flag.NewFlagSet("vm resize", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	vcpu := fs.Int("vcpu", 0, "new vCPU count (required)")
	memoryMB := fs.Int64("memory-mb", 0, "new memory in MB (required)")
	allowMigrate := fs.Bool("allow-migrate", false, "cold resize only: if the VM's current Hypervisor doesn't have room for the new size, move the VM (cold migration) to one that does instead of failing -- the root disk is re-provisioned fresh from the Image on the new Hypervisor (any guest-side changes to it are lost); NetworkInterfaces/VolumeAttachments carry over unchanged. Ignored for a live (Running+CLOUD_HYPERVISOR) resize")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" || *vcpu == 0 || *memoryMB == 0 {
		fatal("-tenant, -id, -vcpu, and -memory-mb are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	vm, err := client.Resize(ctx, &computev1.ResizeVirtualMachineRequest{
		TenantId: *tenant, Id: *id, Vcpu: int32(*vcpu), MemoryMb: *memoryMB, AllowMigrate: *allowMigrate,
	})
	if err != nil {
		fatal("resize: %v", err)
	}
	printVM(vm)
}

// vmMigrate moves a Stopped VM to a different Hypervisor -- cold only, no
// live counterpart (see docs/specs/virtual-machine.md「マイグレーション」):
// its root disk is re-provisioned fresh from its Image there (guest-side
// changes since last boot are lost), while its IP/MAC and Volume data
// carry over unchanged. -target-hypervisor is optional; omit it to let
// the scheduler auto-pick (excluding the VM's current Hypervisor).
func vmMigrate(args []string) {
	fs := flag.NewFlagSet("vm migrate", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	targetHypervisor := fs.String("target-hypervisor", "", "specific Hypervisor to migrate to (default: let the scheduler auto-pick, excluding the VM's current Hypervisor)")
	transferRootDisk := fs.Bool("transfer-root-disk", false, "transfer the VM's actual current root disk content to the new Hypervisor instead of re-cloning a fresh copy from the Image -- costs a real transfer proportional to disk size and requires -migration-registry configured on both Hypervisors (see docs/specs/virtual-machine.md \"ルートディスク転送\")")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	vm, err := client.Migrate(ctx, &computev1.MigrateVirtualMachineRequest{
		TenantId: *tenant, Id: *id, TargetHypervisor: *targetHypervisor, TransferRootDisk: *transferRootDisk,
	})
	if err != nil {
		fatal("migrate: %v", err)
	}
	printVM(vm)
}

// vmAttachVolume/vmDetachVolume attach/detach a Volume to/from a VM --
// cold (Stopped) or live (Running+CLOUD_HYPERVISOR, no downtime), same
// server-side branching as vmResize (see docs/specs/virtual-machine.md).
// No resource_version flag: like vmResize, this does its own
// Get-then-mutate-then-Update server-side.
func vmAttachVolume(args []string) {
	fs := flag.NewFlagSet("vm attach-volume", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	volumeID := fs.String("volume-id", "", "Volume ID to attach (required)")
	deviceHint := fs.String("device-hint", "", "device hint")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" || *volumeID == "" {
		fatal("-tenant, -id, and -volume-id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	vm, err := client.AttachVolume(ctx, &computev1.AttachVolumeRequest{
		TenantId: *tenant, Id: *id, VolumeId: *volumeID, DeviceHint: *deviceHint,
	})
	if err != nil {
		fatal("attach-volume: %v", err)
	}
	printVM(vm)
}

func vmDetachVolume(args []string) {
	fs := flag.NewFlagSet("vm detach-volume", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	volumeID := fs.String("volume-id", "", "Volume ID to detach (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" || *volumeID == "" {
		fatal("-tenant, -id, and -volume-id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	vm, err := client.DetachVolume(ctx, &computev1.DetachVolumeRequest{
		TenantId: *tenant, Id: *id, VolumeId: *volumeID,
	})
	if err != nil {
		fatal("detach-volume: %v", err)
	}
	printVM(vm)
}

// vmReboot implements reboot/hard-reboot client-side, as Stop (waiting for
// Stopped) followed by Start -- there is no separate server-side RPC or
// state for either (see docs/architecture.md's VM lifecycle): the server
// only ever knows Stop and Start. hardForce=true passes force=true to Stop
// (immediate SIGKILL) instead of the default graceful shutdown.
func vmReboot(args []string, hardForce bool) {
	name := "vm reboot"
	if hardForce {
		name = "vm hard-reboot"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)

	if _, err := client.Stop(ctx, &computev1.StopVirtualMachineRequest{TenantId: *tenant, Id: *id, Force: hardForce}); err != nil {
		fatal("stop: %v", err)
	}
	waitForPhase(ctx, client, *tenant, *id, "Stopped")

	vm, err := client.Start(ctx, &computev1.StartVirtualMachineRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("start: %v", err)
	}
	printVM(vm)
}

// waitForPhase polls Get until id reaches wantPhase, same "one-shot CLI
// tool, not a controller loop" simplicity as vmAddFinalizer's doc comment --
// Watch (as waitForTerminal uses) would work too, but a plain poll is
// simpler here and this is not a hot path.
func waitForPhase(ctx context.Context, client computev1.VirtualMachineServiceClient, tenant, id, wantPhase string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		vm, err := client.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: tenant, Id: id})
		if err != nil {
			fatal("get: %v", err)
		}
		phase := vm.GetStatus().GetPhase()
		if phase == wantPhase {
			return
		}
		fmt.Fprintf(os.Stderr, "... phase=%s (waiting for %s)\n", phase, wantPhase)
		select {
		case <-ctx.Done():
			fatal("timed out waiting for phase=%s (last seen: %s)", wantPhase, phase)
		case <-time.After(time.Second):
		}
	}
}

func vmList(args []string) {
	fs := flag.NewFlagSet("vm list", flag.ExitOnError)
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
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &computev1.ListVirtualMachinesRequest{TenantId: *tenant})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, vm := range resp.GetItems() {
		printVM(vm)
	}
}

func vmWatch(args []string) {
	fs := flag.NewFlagSet("vm watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required unless -all-tenants)")
	allTenants := fs.Bool("all-tenants", false, "every tenant at once (empty tenant_id); needs a cross-tenant role -- see docs/specs/authn-authz.md")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	finalizerName := fs.String("finalizer-name", "", "only watch VMs whose finalizers currently include this name, instead of every VM in the tenant (see docs/specs/external-integration.md)")
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
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &computev1.WatchVirtualMachinesRequest{
		TenantId:             *tenant,
		SinceResourceVersion: *since,
		FinalizerName:        *finalizerName,
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
		if ev.GetType() == computev1.VirtualMachineEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		vm := ev.GetVm()
		fmt.Printf("%-10s %-24s phase=%-12s hypervisor=%s rv=%d\n",
			ev.GetType(), vm.GetMeta().GetId(), vm.GetStatus().GetPhase(), vm.GetStatus().GetHypervisor(), ev.GetResourceVersion())
	}
}

// vmConsole streams a VM's serial console (see docs/specs/firecracker-boot.md)
// straight to stdout as raw bytes -- no framing, so it's pipeable/pageable
// like any other log. -follow keeps it open for new output, like `tail -f`;
// without it, the command exits once existing history has been replayed.
func vmConsole(args []string) {
	fs := flag.NewFlagSet("vm console", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	tailBytes := fs.Int64("tail-bytes", 0, "trailing bytes of existing console output to replay (0: server default ~64KiB; negative: entire log)")
	follow := fs.Bool("follow", false, "keep streaming new console output after replaying history, like tail -f")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.StreamConsole(ctx, &computev1.StreamConsoleRequest{
		TenantId:  *tenant,
		Id:        *id,
		TailBytes: *tailBytes,
		Follow:    *follow,
	})
	if err != nil {
		fatal("console: %v", err)
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			fatal("console: %v", err)
		}
		os.Stdout.Write(chunk.GetData())
	}
}

func printVM(vm *computev1.VirtualMachine) {
	finalizerNames := make([]string, len(vm.GetMeta().GetFinalizers()))
	for i, f := range vm.GetMeta().GetFinalizers() {
		finalizerNames[i] = f.GetName()
	}
	numaNode := "-"
	if n := vm.GetStatus().GetAllocatedNumaNode(); n >= 0 {
		numaNode = strconv.Itoa(int(n))
	}
	fmt.Printf("id=%s name=%s tenant=%s labels=%s phase=%s hypervisor=%s interfaces=%s pci_devices=%s numa_node=%s finalizers=%s deleted_at=%s rv=%d\n",
		vm.GetMeta().GetId(), vm.GetMeta().GetName(), vm.GetMeta().GetTenantId(), formatKeyValues(vm.GetMeta().GetLabels()),
		vm.GetStatus().GetPhase(), vm.GetStatus().GetHypervisor(),
		strings.Join(vm.GetStatus().GetInterfaceRefs(), ","),
		strings.Join(vm.GetStatus().GetAllocatedPciDevices(), ","),
		numaNode,
		strings.Join(finalizerNames, ","), deletedAtString(vm.GetMeta().GetDeletedAt()),
		vm.GetMeta().GetResourceVersion())
}

// parseKeyValues parses a "k=v,k2=v2" flag value into a map (nil for "").
func parseKeyValues(flagName, s string) map[string]string {
	if s == "" {
		return nil
	}
	out := make(map[string]string)
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			fatal("%s: %q is not key=value", flagName, kv)
		}
		out[k] = v
	}
	return out
}

// formatKeyValues is parseKeyValues' inverse, sorted by key for stable output.
func formatKeyValues(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// formatIntValues renders allocated named values ("vlan_id=100,vni=5").
func formatIntValues(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, ",")
}

// pendingReason appends a still-true AllocationPending/NoFreeAddress
// condition's message, so a stuck resource says why.
func pendingReason(conds []*resourcev1.Condition) string {
	for _, c := range conds {
		if (c.GetType() == "AllocationPending" || c.GetType() == "NoFreeAddress") && c.GetStatus() == "True" && c.GetMessage() != "" {
			return fmt.Sprintf(" pending_reason=%q", c.GetMessage())
		}
	}
	return ""
}

func deletedAtString(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().Format(time.RFC3339)
}

// formatConditions renders conditions as a compact "Type=Status,..." list
// (e.g. "IdentifierVerified=True,SizeMatchesDeclaration=False") -- shared by
// every resource that carries resource.v1.Condition (Volume, Subnet,
// StorageConnection, ...), none of which surfaced Conditions in the CLI at
// all before this.
func formatConditions(conditions []*resourcev1.Condition) string {
	parts := make([]string, len(conditions))
	for i, c := range conditions {
		parts[i] = c.GetType() + "=" + c.GetStatus()
	}
	return strings.Join(parts, ",")
}

// vmAddFinalizer/vmRemoveFinalizer implement docs/architecture.md
// "Finalizer": an external controller registers or clears its own holder
// name in meta.finalizers via a plain Get-then-Update, exactly like any
// other VM field mutation -- there's no dedicated RPC for this, Finalizers
// is just another part of ObjectMeta. No retry-on-conflict here (unlike
// e.g. compute's internal updateHypervisor): this is a one-shot CLI tool,
// not a controller loop, so a resource_version conflict is simply reported
// to the caller to retry themselves.
func vmAddFinalizer(args []string) {
	fs := flag.NewFlagSet("vm add-finalizer", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	finalizer := fs.String("finalizer", "", "holder name to add, e.g. \"acme.corp/network-acl-cleanup\" (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" || *finalizer == "" {
		fatal("-tenant, -id, and -finalizer are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)

	vm, err := client.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	for _, f := range vm.GetMeta().GetFinalizers() {
		if f.GetName() == *finalizer {
			printVM(vm) // already present: idempotent no-op
			return
		}
	}
	vm.Meta.Finalizers = append(vm.Meta.Finalizers, &resourcev1.Finalizer{Name: *finalizer})
	updated, err := client.Update(ctx, &computev1.UpdateVirtualMachineRequest{TenantId: *tenant, Vm: vm})
	if err != nil {
		fatal("update: %v", err)
	}
	printVM(updated)
}

func vmRemoveFinalizer(args []string) {
	fs := flag.NewFlagSet("vm remove-finalizer", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	finalizer := fs.String("finalizer", "", "holder name to remove (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" || *finalizer == "" {
		fatal("-tenant, -id, and -finalizer are required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)

	vm, err := client.Get(ctx, &computev1.GetVirtualMachineRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	kept := vm.Meta.Finalizers[:0]
	for _, f := range vm.GetMeta().GetFinalizers() {
		if f.GetName() != *finalizer {
			kept = append(kept, f)
		}
	}
	vm.Meta.Finalizers = kept
	updated, err := client.Update(ctx, &computev1.UpdateVirtualMachineRequest{TenantId: *tenant, Vm: vm})
	if err != nil {
		fatal("update: %v", err)
	}
	printVM(updated)
}

func parseVmmDriver(s string) computev1.VmmDriver {
	switch s {
	case "":
		return computev1.VmmDriver_VMM_DRIVER_UNSPECIFIED
	case "firecracker":
		return computev1.VmmDriver_VMM_DRIVER_FIRECRACKER
	case "cloud-hypervisor":
		return computev1.VmmDriver_VMM_DRIVER_CLOUD_HYPERVISOR
	default:
		// Fatal, not a silent fallback: unlike -recovery-policy, silently
		// defaulting an unrecognized value here (e.g. a typo'd
		// "CLOUD-HYPERVISOR") would pick a different, working driver rather
		// than obviously failing -- a KERNEL_ROOTFS Image accepts either, so
		// nothing would complain.
		fatal("-driver-hint: unrecognized %q, want firecracker|cloud-hypervisor", s)
		return computev1.VmmDriver_VMM_DRIVER_UNSPECIFIED
	}
}
