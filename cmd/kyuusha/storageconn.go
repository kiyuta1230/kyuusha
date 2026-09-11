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

	blockstoragev1 "gitlab.com/ki.yuta1230/kyuusha/gen/go/kyuusha/blockstorage/v1"
)

func dialStorageConnections(addr string) blockstoragev1.StorageConnectionServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return blockstoragev1.NewStorageConnectionServiceClient(conn)
}

func storageConnCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		storageConnCreate(args[1:])
	case "get":
		storageConnGet(args[1:])
	case "list":
		storageConnList(args[1:])
	case "watch":
		storageConnWatch(args[1:])
	case "delete":
		storageConnDelete(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func storageConnCreate(args []string) {
	fs := flag.NewFlagSet("storageconn create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN; admin-only)")
	name := fs.String("name", "", "storage connection name (idempotency key; required) -- must match the name Hypervisors declare in their own -storage-connections flag, and what a Volume's -storage-connection references")
	zones := fs.String("zones", "", "comma-separated Availability Zones this storage backend may be connected to from (required) -- physical reality (single-AZ, or replicated/reachable across several) is up to the storage admin to know, not kyuusha")
	annotations := fs.String("annotations", "", "comma-separated key=value pairs, never interpreted by kyuusha itself -- purely a reference field for admins/users (e.g. the storage product/version)")
	fs.Parse(args)

	if *name == "" || *zones == "" {
		fatal("-name and -zones are required")
	}

	client := dialStorageConnections(*addr)
	ctx := authedContext(context.Background(), *token)

	sc, err := client.Create(ctx, &blockstoragev1.CreateStorageConnectionRequest{
		Name: *name,
		Spec: &blockstoragev1.StorageConnectionSpec{
			Zones:       strings.Split(*zones, ","),
			Annotations: parseAnnotations(*annotations),
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printStorageConnection(sc)
}

func storageConnGet(args []string) {
	fs := flag.NewFlagSet("storageconn get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN; admin-only)")
	id := fs.String("id", "", "storage connection ID (required)")
	fs.Parse(args)

	if *id == "" {
		fatal("-id is required")
	}
	client := dialStorageConnections(*addr)
	ctx := authedContext(context.Background(), *token)
	sc, err := client.Get(ctx, &blockstoragev1.GetStorageConnectionRequest{Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printStorageConnection(sc)
}

func storageConnList(args []string) {
	fs := flag.NewFlagSet("storageconn list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN; admin-only)")
	fs.Parse(args)

	client := dialStorageConnections(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &blockstoragev1.ListStorageConnectionsRequest{})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, sc := range resp.GetItems() {
		printStorageConnection(sc)
	}
}

func storageConnWatch(args []string) {
	fs := flag.NewFlagSet("storageconn watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN; admin-only)")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)

	client := dialStorageConnections(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &blockstoragev1.WatchStorageConnectionsRequest{SinceResourceVersion: *since})
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
		if ev.GetType() == blockstoragev1.StorageConnectionEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		sc := ev.GetStorageConnection()
		fmt.Printf("%-10s %-24s phase=%-10s verified_zones=%v rv=%d\n",
			ev.GetType(), sc.GetMeta().GetId(), sc.GetStatus().GetPhase(), sc.GetStatus().GetVerifiedZones(), ev.GetResourceVersion())
	}
}

func storageConnDelete(args []string) {
	fs := flag.NewFlagSet("storageconn delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN; admin-only)")
	id := fs.String("id", "", "storage connection ID (required)")
	fs.Parse(args)

	if *id == "" {
		fatal("-id is required")
	}
	client := dialStorageConnections(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &blockstoragev1.DeleteStorageConnectionRequest{Id: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func printStorageConnection(sc *blockstoragev1.StorageConnection) {
	fmt.Printf("id=%s name=%s zones=%v phase=%s verified_zones=%v rv=%d\n",
		sc.GetMeta().GetId(), sc.GetMeta().GetName(), sc.GetSpec().GetZones(),
		sc.GetStatus().GetPhase(), sc.GetStatus().GetVerifiedZones(), sc.GetMeta().GetResourceVersion())
}
