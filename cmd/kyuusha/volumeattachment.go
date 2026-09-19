package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

func dialVolumeAttachments(addr string) blockstoragev1.VolumeAttachmentServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return blockstoragev1.NewVolumeAttachmentServiceClient(conn)
}

func volattachCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		volattachCreate(args[1:])
	case "get":
		volattachGet(args[1:])
	case "list":
		volattachList(args[1:])
	case "watch":
		volattachWatch(args[1:])
	case "delete":
		volattachDelete(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

// volattachCreate talks to block-storage directly (see dialVolumeAttachments)
// and never touches compute: it can reach VolumeAttachmentStatus.Phase
// Attached without ever attaching anything on the actual VMM if -vm names a
// VM that's already booted, or -- for one that's Stopped -- without the
// attachment ever taking effect on a later Start, since it doesn't touch
// that VM's spec.volumes either (see docs/specs/volume.md「compute側の統合」).
// Prefer `kyuusha vm attach-volume`/`detach-volume` (vm.go) for attaching a
// Volume to a VM you actually want it to reach; this command remains as the
// low-level primitive createVolumeAttachments itself uses internally.
func volattachCreate(args []string) {
	fs := flag.NewFlagSet("volattach create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "volume attachment name (idempotency key)")
	vmID := fs.String("vm", "", "VM ID (required)")
	volumeID := fs.String("volume", "", "volume ID (required)")
	deviceHint := fs.String("device-hint", "", "requested device path (optional)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *vmID == "" || *volumeID == "" {
		fatal("-tenant, -vm, and -volume are required")
	}

	client := dialVolumeAttachments(*addr)
	ctx := authedContext(context.Background(), *token)

	a, err := client.Create(ctx, &blockstoragev1.CreateVolumeAttachmentRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec: &blockstoragev1.VolumeAttachmentSpec{
			VmId:       *vmID,
			VolumeId:   *volumeID,
			DeviceHint: *deviceHint,
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printVolumeAttachment(a)
}

func volattachGet(args []string) {
	fs := flag.NewFlagSet("volattach get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "volume attachment ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialVolumeAttachments(*addr)
	ctx := authedContext(context.Background(), *token)
	a, err := client.Get(ctx, &blockstoragev1.GetVolumeAttachmentRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printVolumeAttachment(a)
}

func volattachList(args []string) {
	fs := flag.NewFlagSet("volattach list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dialVolumeAttachments(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &blockstoragev1.ListVolumeAttachmentsRequest{TenantId: *tenant})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, a := range resp.GetItems() {
		printVolumeAttachment(a)
	}
}

func volattachWatch(args []string) {
	fs := flag.NewFlagSet("volattach watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dialVolumeAttachments(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &blockstoragev1.WatchVolumeAttachmentsRequest{
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
		if ev.GetType() == blockstoragev1.VolumeAttachmentEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		a := ev.GetVolumeAttachment()
		fmt.Printf("%-10s %-24s phase=%-10s rv=%d\n",
			ev.GetType(), a.GetMeta().GetId(), a.GetStatus().GetPhase(), ev.GetResourceVersion())
	}
}

func volattachDelete(args []string) {
	fs := flag.NewFlagSet("volattach delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "volume attachment ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialVolumeAttachments(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &blockstoragev1.DeleteVolumeAttachmentRequest{TenantId: *tenant, Id: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func printVolumeAttachment(a *blockstoragev1.VolumeAttachment) {
	fmt.Printf("id=%s name=%s tenant=%s vm=%s volume=%s phase=%s hypervisor=%s device_path=%s rv=%d\n",
		a.GetMeta().GetId(), a.GetMeta().GetName(), a.GetMeta().GetTenantId(),
		a.GetSpec().GetVmId(), a.GetSpec().GetVolumeId(),
		a.GetStatus().GetPhase(), a.GetStatus().GetHypervisor(), a.GetStatus().GetDevicePath(),
		a.GetMeta().GetResourceVersion())
}
