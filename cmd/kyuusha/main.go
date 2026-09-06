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
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
	identityv1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/identity/v1"
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
	case "image":
		imageCmd(os.Args[2:])
	case "subnet":
		subnetCmd(os.Args[2:])
	case "netif":
		netifCmd(os.Args[2:])
	case "volume":
		volumeCmd(os.Args[2:])
	case "volattach":
		volattachCmd(os.Args[2:])
	case "token":
		tokenCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  kyuusha vm <create|get|list|watch|console> [flags]
  kyuusha tenant <create|get|list|watch> [flags]
  kyuusha hypervisor <get|list|watch|set-schedulable> [flags]   (admin-only)
  kyuusha image <create|get|list|watch> [flags]
  kyuusha subnet <create|get|list|watch> [flags]
  kyuusha netif <create|get|list|watch> [flags]
  kyuusha volume <create|get|list|watch> [flags]
  kyuusha volattach <create|get|list|watch> [flags]
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
	image := fs.String("image", "", "image ID (required)")
	vcpu := fs.Int("vcpu", 1, "vCPU count")
	memoryMB := fs.Int64("memory-mb", 1024, "memory in MB")
	recovery := fs.String("recovery-policy", "none", "none|self-heal")
	subnets := fs.String("subnets", "", "comma-separated subnet IDs to attach network interfaces to (first one is primary); all must be in the same zone")
	wait := fs.Bool("wait", false, "block until the VM reaches Running or Error")
	fs.Parse(args)

	if *tenant == "" || *image == "" {
		fatal("-tenant and -image are required")
	}

	var netifs []*computev1.NetworkAttachment
	for i, subnetID := range strings.Split(*subnets, ",") {
		if subnetID == "" {
			continue
		}
		netifs = append(netifs, &computev1.NetworkAttachment{SubnetId: subnetID, Primary: i == 0})
	}

	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)

	vm, err := client.Create(ctx, &computev1.CreateVirtualMachineRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec: &computev1.VirtualMachineSpec{
			ImageId:           *image,
			Vcpu:              int32(*vcpu),
			MemoryMb:          *memoryMB,
			RecoveryPolicy:    parseRecoveryPolicy(*recovery),
			NetworkInterfaces: netifs,
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

func vmList(args []string) {
	fs := flag.NewFlagSet("vm list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	fs.Parse(args)

	if *tenant == "" {
		fatal("-tenant is required")
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
	tenant := fs.String("tenant", "", "tenant ID (required)")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dial(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &computev1.WatchVirtualMachinesRequest{
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
	fmt.Printf("id=%s name=%s tenant=%s phase=%s hypervisor=%s interfaces=%s rv=%d\n",
		vm.GetMeta().GetId(), vm.GetMeta().GetName(), vm.GetMeta().GetTenantId(),
		vm.GetStatus().GetPhase(), vm.GetStatus().GetHypervisor(),
		strings.Join(vm.GetStatus().GetInterfaceRefs(), ","), vm.GetMeta().GetResourceVersion())
}

func parseRecoveryPolicy(s string) computev1.RecoveryPolicy {
	switch s {
	case "self-heal":
		return computev1.RecoveryPolicy_RECOVERY_POLICY_SELF_HEAL
	default:
		return computev1.RecoveryPolicy_RECOVERY_POLICY_NONE
	}
}
