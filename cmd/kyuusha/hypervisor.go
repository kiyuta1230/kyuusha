package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	computev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/compute/v1"
)

// hypervisorCmd covers Get/List/Watch/SetSchedulable: Hypervisor is
// compute's internal scheduling inventory, not a KaaS-facing resource.
// Registration happens automatically when compute-agent starts -- there is
// no `create` here. Admin-only through api-gateway (see internal/gateway's
// HypervisorProxy doc comment).
func hypervisorCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "get":
		hypervisorGet(args[1:])
	case "list":
		hypervisorList(args[1:])
	case "watch":
		hypervisorWatch(args[1:])
	case "set-schedulable":
		hypervisorSetSchedulable(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func hypervisorGet(args []string) {
	fs := flag.NewFlagSet("hypervisor get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN); must carry role=admin")
	id := fs.String("id", "", "hypervisor ID (required)")
	fs.Parse(args)

	if *id == "" {
		fatal("-id is required")
	}
	client := dialHypervisors(*addr)
	ctx := authedContext(context.Background(), *token)
	h, err := client.Get(ctx, &computev1.GetHypervisorRequest{Hypervisor: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printHypervisor(h)
}

func hypervisorList(args []string) {
	fs := flag.NewFlagSet("hypervisor list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN); must carry role=admin")
	fs.Parse(args)

	client := dialHypervisors(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &computev1.ListHypervisorsRequest{})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, h := range resp.GetItems() {
		printHypervisor(h)
	}
}

func hypervisorWatch(args []string) {
	fs := flag.NewFlagSet("hypervisor watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN); must carry role=admin")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)

	client := dialHypervisors(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &computev1.WatchHypervisorsRequest{SinceResourceVersion: *since})
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
		if ev.GetType() == computev1.HypervisorEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		h := ev.GetHypervisor()
		fmt.Printf("%-10s %-16s phase=%-10s schedulable=%-5t zone=%-10s allocated=%d/%dvcpu %d/%dMB rv=%d\n",
			ev.GetType(), h.GetMeta().GetId(), h.GetStatus().GetPhase(), h.GetSpec().GetSchedulable(), h.GetStatus().GetZone(),
			h.GetStatus().GetAllocatedVcpu(), h.GetStatus().GetAllocatableVcpu(),
			h.GetStatus().GetAllocatedMemoryMb(), h.GetStatus().GetAllocatableMemoryMb(),
			ev.GetResourceVersion())
	}
}

// hypervisorSetSchedulable marks a Hypervisor schedulable or not, for
// planned maintenance -- independent of its heartbeat-derived phase.
func hypervisorSetSchedulable(args []string) {
	fs := flag.NewFlagSet("hypervisor set-schedulable", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN); must carry role=admin")
	id := fs.String("id", "", "hypervisor ID (required)")
	schedulable := fs.Bool("schedulable", true, "false excludes it from scheduling regardless of phase (maintenance)")
	fs.Parse(args)

	if *id == "" {
		fatal("-id is required")
	}
	client := dialHypervisors(*addr)
	ctx := authedContext(context.Background(), *token)
	h, err := client.SetSchedulable(ctx, &computev1.SetSchedulableRequest{Hypervisor: *id, Schedulable: *schedulable})
	if err != nil {
		fatal("set-schedulable: %v", err)
	}
	printHypervisor(h)
}

func printHypervisor(h *computev1.Hypervisor) {
	st := h.GetStatus()
	fmt.Printf("id=%s phase=%s schedulable=%t zone=%s drivers=%v allocated=%d/%dvcpu %d/%dMB rv=%d\n",
		h.GetMeta().GetId(), st.GetPhase(), h.GetSpec().GetSchedulable(), st.GetZone(), st.GetSupportedDrivers(),
		st.GetAllocatedVcpu(), st.GetAllocatableVcpu(),
		st.GetAllocatedMemoryMb(), st.GetAllocatableMemoryMb(),
		h.GetMeta().GetResourceVersion())
}
