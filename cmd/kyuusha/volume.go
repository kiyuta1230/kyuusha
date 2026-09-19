package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	blockstoragev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

func dialVolumes(addr string) blockstoragev1.VolumeServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return blockstoragev1.NewVolumeServiceClient(conn)
}

func volumeCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		volumeCreate(args[1:])
	case "get":
		volumeGet(args[1:])
	case "list":
		volumeList(args[1:])
	case "watch":
		volumeWatch(args[1:])
	case "delete":
		volumeDelete(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func volumeCreate(args []string) {
	fs := flag.NewFlagSet("volume create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "volume name (idempotency key)")
	sizeGB := fs.Int64("size-gb", 0, "volume size in GB, self-reported for quota only -- kyuusha never provisions storage, so it can't verify this (required)")
	protocol := fs.String("protocol", "", "protocol this Volume is reachable over: ISCSI, NVME_OF, or NFS (required)")
	storageConnection := fs.String("storage-connection", "", "name of the Hypervisor-side storage connection this Volume lives behind -- must match a StorageConnection a Hypervisor declared at registration (required)")
	identifier := fs.String("identifier", "", "protocol-specific identifier: for ISCSI/NVME_OF, the block device's stable name under /dev/disk/by-id/; for NFS, a path relative to the connection's mount point (required)")
	annotations := fs.String("annotations", "", "comma-separated key=value pairs, never interpreted by kyuusha itself -- purely a reference field for admins/users (e.g. a QoS tier)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *sizeGB == 0 || *protocol == "" || *storageConnection == "" || *identifier == "" {
		fatal("-tenant, -size-gb, -protocol, -storage-connection, and -identifier are required")
	}
	protoVal, ok := blockstoragev1.StorageProtocol_value[*protocol]
	if !ok {
		fatal("-protocol must be ISCSI, NVME_OF, or NFS, got %q", *protocol)
	}

	client := dialVolumes(*addr)
	ctx := authedContext(context.Background(), *token)

	vol, err := client.Create(ctx, &blockstoragev1.CreateVolumeRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec: &blockstoragev1.VolumeSpec{
			SizeGb:            *sizeGB,
			Protocol:          blockstoragev1.StorageProtocol(protoVal),
			StorageConnection: *storageConnection,
			Identifier:        *identifier,
			Annotations:       parseAnnotations(*annotations),
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printVolume(vol)
}

func volumeGet(args []string) {
	fs := flag.NewFlagSet("volume get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "volume ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialVolumes(*addr)
	ctx := authedContext(context.Background(), *token)
	vol, err := client.Get(ctx, &blockstoragev1.GetVolumeRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printVolume(vol)
}

func volumeList(args []string) {
	fs := flag.NewFlagSet("volume list", flag.ExitOnError)
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
	client := dialVolumes(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &blockstoragev1.ListVolumesRequest{TenantId: *tenant})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, vol := range resp.GetItems() {
		printVolume(vol)
	}
}

func volumeWatch(args []string) {
	fs := flag.NewFlagSet("volume watch", flag.ExitOnError)
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
	client := dialVolumes(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &blockstoragev1.WatchVolumesRequest{
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
		if ev.GetType() == blockstoragev1.VolumeEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		vol := ev.GetVolume()
		fmt.Printf("%-10s %-24s phase=%-10s rv=%d\n",
			ev.GetType(), vol.GetMeta().GetId(), vol.GetStatus().GetPhase(), ev.GetResourceVersion())
	}
}

func volumeDelete(args []string) {
	fs := flag.NewFlagSet("volume delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "volume ID (required)")
	fs.Parse(args)
	if *tenant == "" {
		*tenant = resolveTenant(*token)
	}

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialVolumes(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &blockstoragev1.DeleteVolumeRequest{TenantId: *tenant, Id: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func printVolume(vol *blockstoragev1.Volume) {
	fmt.Printf("id=%s name=%s tenant=%s size_gb=%d protocol=%s storage_connection=%s identifier=%s phase=%s conditions=%s rv=%d\n",
		vol.GetMeta().GetId(), vol.GetMeta().GetName(), vol.GetMeta().GetTenantId(),
		vol.GetSpec().GetSizeGb(), vol.GetSpec().GetProtocol(), vol.GetSpec().GetStorageConnection(), vol.GetSpec().GetIdentifier(),
		vol.GetStatus().GetPhase(), formatConditions(vol.GetStatus().GetConditions()), vol.GetMeta().GetResourceVersion())
}

// parseAnnotations turns "-annotations=k1=v1,k2=v2" into a map -- shared by
// volume.go and storageconn.go, since both StorageConnection and Volume
// carry the exact same kyuusha-opaque reference field.
func parseAnnotations(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		out[k] = v
	}
	return out
}
