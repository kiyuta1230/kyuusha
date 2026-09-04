// Command kyuusha is the CLI client for the kyuusha services. It is a thin
// wrapper over the gRPC API; see docs/architecture.md "API消費者の多様化に
// 備える". Currently only `vm` (VirtualMachineService) is wired up.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "vm":
		vmCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: kyuusha vm <create|get|list|watch> [flags]`)
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func vmCreate(args []string) {
	fs := flag.NewFlagSet("vm create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8081", "compute service address")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "VM name (idempotency key)")
	image := fs.String("image", "", "image ID (required)")
	vcpu := fs.Int("vcpu", 1, "vCPU count")
	memoryMB := fs.Int64("memory-mb", 1024, "memory in MB")
	recovery := fs.String("recovery-policy", "none", "none|self-heal")
	wait := fs.Bool("wait", false, "block until the VM reaches Running or Error")
	fs.Parse(args)

	if *tenant == "" || *image == "" {
		fatal("-tenant and -image are required")
	}

	client := dial(*addr)
	ctx := context.Background()

	vm, err := client.Create(ctx, &computev1.CreateVirtualMachineRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec: &computev1.VirtualMachineSpec{
			ImageId:        *image,
			Vcpu:           int32(*vcpu),
			MemoryMb:       *memoryMB,
			RecoveryPolicy: parseRecoveryPolicy(*recovery),
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
	addr := fs.String("addr", "localhost:8081", "compute service address")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "VM ID (required)")
	fs.Parse(args)

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dial(*addr)
	vm, err := client.Get(context.Background(), &computev1.GetVirtualMachineRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printVM(vm)
}

func vmList(args []string) {
	fs := flag.NewFlagSet("vm list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8081", "compute service address")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	fs.Parse(args)

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dial(*addr)
	resp, err := client.List(context.Background(), &computev1.ListVirtualMachinesRequest{TenantId: *tenant})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, vm := range resp.GetItems() {
		printVM(vm)
	}
}

func vmWatch(args []string) {
	fs := flag.NewFlagSet("vm watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8081", "compute service address")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dial(*addr)
	stream, err := client.Watch(context.Background(), &computev1.WatchVirtualMachinesRequest{
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
		fmt.Printf("%-10s %-24s phase=%-12s node=%s rv=%d\n",
			ev.GetType(), vm.GetMeta().GetId(), vm.GetStatus().GetPhase(), vm.GetStatus().GetNode(), ev.GetResourceVersion())
	}
}

func printVM(vm *computev1.VirtualMachine) {
	fmt.Printf("id=%s name=%s tenant=%s phase=%s node=%s rv=%d\n",
		vm.GetMeta().GetId(), vm.GetMeta().GetName(), vm.GetMeta().GetTenantId(),
		vm.GetStatus().GetPhase(), vm.GetStatus().GetNode(), vm.GetMeta().GetResourceVersion())
}

func parseRecoveryPolicy(s string) computev1.RecoveryPolicy {
	switch s {
	case "self-heal":
		return computev1.RecoveryPolicy_RECOVERY_POLICY_SELF_HEAL
	default:
		return computev1.RecoveryPolicy_RECOVERY_POLICY_NONE
	}
}
