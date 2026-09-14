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

	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

func dialImages(addr string) imagev1.ImageServiceClient {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial %s: %v", addr, err)
	}
	return imagev1.NewImageServiceClient(conn)
}

func imageCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		imageCreate(args[1:])
	case "build":
		imageBuild(args[1:])
	case "get":
		imageGet(args[1:])
	case "list":
		imageList(args[1:])
	case "watch":
		imageWatch(args[1:])
	case "share":
		imageShare(args[1:])
	case "delete":
		imageDelete(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func imageCreate(args []string) {
	fs := flag.NewFlagSet("image create", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "image name (idempotency key)")
	format := fs.String("format", "", "kernel_rootfs|qcow2 (required)")
	kernelURL := fs.String("kernel-url", "", "kernel artifact URL (kernel_rootfs)")
	kernelDigest := fs.String("kernel-digest", "", "kernel artifact digest, e.g. sha256:... (kernel_rootfs)")
	rootfsURL := fs.String("rootfs-url", "", "rootfs artifact URL (kernel_rootfs)")
	rootfsDigest := fs.String("rootfs-digest", "", "rootfs artifact digest (kernel_rootfs)")
	diskURL := fs.String("disk-url", "", "disk artifact URL (qcow2)")
	diskDigest := fs.String("disk-digest", "", "disk artifact digest (qcow2)")
	bootArgs := fs.String("boot-args", "", "direct kernel boot arguments (kernel_rootfs)")
	visibility := fs.String("visibility", "private", "private|public (default private)")
	sharedWith := fs.String("shared-with-tenant-ids", "", "comma-separated tenant IDs allowed to see/reference this Image (private only)")
	fs.Parse(args)

	if *tenant == "" || *format == "" {
		fatal("-tenant and -format are required")
	}

	client := dialImages(*addr)
	ctx := authedContext(context.Background(), *token)

	img, err := client.Create(ctx, &imagev1.CreateImageRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec: &imagev1.ImageSpec{
			Format:              parseImageFormat(*format),
			Kernel:              &imagev1.ImageArtifact{Url: *kernelURL, Digest: *kernelDigest},
			Rootfs:              &imagev1.ImageArtifact{Url: *rootfsURL, Digest: *rootfsDigest},
			Disk:                &imagev1.ImageArtifact{Url: *diskURL, Digest: *diskDigest},
			BootArgs:            *bootArgs,
			Visibility:          parseImageVisibility(*visibility),
			SharedWithTenantIds: splitNonEmpty(*sharedWith),
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printImage(img)
}

func imageGet(args []string) {
	fs := flag.NewFlagSet("image get", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "image ID (required)")
	fs.Parse(args)

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialImages(*addr)
	ctx := authedContext(context.Background(), *token)
	img, err := client.Get(ctx, &imagev1.GetImageRequest{TenantId: *tenant, Id: *id})
	if err != nil {
		fatal("get: %v", err)
	}
	printImage(img)
}

func imageList(args []string) {
	fs := flag.NewFlagSet("image list", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	fs.Parse(args)

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dialImages(*addr)
	ctx := authedContext(context.Background(), *token)
	resp, err := client.List(ctx, &imagev1.ListImagesRequest{TenantId: *tenant})
	if err != nil {
		fatal("list: %v", err)
	}
	for _, img := range resp.GetItems() {
		printImage(img)
	}
}

func imageWatch(args []string) {
	fs := flag.NewFlagSet("image watch", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	since := fs.Int64("since-resource-version", 0, "resume from this resource_version")
	fs.Parse(args)

	if *tenant == "" {
		fatal("-tenant is required")
	}
	client := dialImages(*addr)
	ctx := authedContext(context.Background(), *token)
	stream, err := client.Watch(ctx, &imagev1.WatchImagesRequest{
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
		if ev.GetType() == imagev1.ImageEvent_BOOKMARK {
			fmt.Printf("BOOKMARK resource_version=%d\n", ev.GetResourceVersion())
			continue
		}
		img := ev.GetImage()
		fmt.Printf("%-10s %-24s phase=%-10s rv=%d\n",
			ev.GetType(), img.GetMeta().GetId(), img.GetStatus().GetPhase(), ev.GetResourceVersion())
	}
}

// imageShare calls SetVisibility, the narrow, purpose-built mutation for
// spec.visibility/shared_with_tenant_ids -- Image has no general Update RPC
// (kernel/rootfs/disk URLs are meant to be immutable once Created; see
// docs/specs/image.md). Only the owning tenant may call this.
func imageShare(args []string) {
	fs := flag.NewFlagSet("image share", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required; must be the owning tenant)")
	id := fs.String("id", "", "image ID (required)")
	visibility := fs.String("visibility", "private", "private|public (default private)")
	sharedWith := fs.String("shared-with-tenant-ids", "", "comma-separated tenant IDs allowed to see/reference this Image (private only); replaces the existing list entirely")
	fs.Parse(args)

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialImages(*addr)
	ctx := authedContext(context.Background(), *token)
	img, err := client.SetVisibility(ctx, &imagev1.SetImageVisibilityRequest{
		TenantId:            *tenant,
		Id:                  *id,
		Visibility:          parseImageVisibility(*visibility),
		SharedWithTenantIds: splitNonEmpty(*sharedWith),
	})
	if err != nil {
		fatal("share: %v", err)
	}
	printImage(img)
}

func imageDelete(args []string) {
	fs := flag.NewFlagSet("image delete", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN); must be the owning tenant")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	id := fs.String("id", "", "image ID (required)")
	fs.Parse(args)

	if *tenant == "" || *id == "" {
		fatal("-tenant and -id are required")
	}
	client := dialImages(*addr)
	ctx := authedContext(context.Background(), *token)
	if _, err := client.Delete(ctx, &imagev1.DeleteImageRequest{TenantId: *tenant, Id: *id}); err != nil {
		fatal("delete: %v", err)
	}
}

func printImage(img *imagev1.Image) {
	fmt.Printf("id=%s name=%s tenant=%s format=%s phase=%s visibility=%s shared_with=%s rv=%d\n",
		img.GetMeta().GetId(), img.GetMeta().GetName(), img.GetMeta().GetTenantId(),
		img.GetSpec().GetFormat(), img.GetStatus().GetPhase(), img.GetSpec().GetVisibility(),
		strings.Join(img.GetSpec().GetSharedWithTenantIds(), ","), img.GetMeta().GetResourceVersion())
}

func parseImageFormat(s string) imagev1.ImageFormat {
	switch s {
	case "kernel_rootfs":
		return imagev1.ImageFormat_KERNEL_ROOTFS
	case "qcow2":
		return imagev1.ImageFormat_QCOW2
	default:
		fatal("-format must be kernel_rootfs or qcow2, got %q", s)
		return imagev1.ImageFormat_IMAGE_FORMAT_UNSPECIFIED
	}
}

func parseImageVisibility(s string) imagev1.ImageVisibility {
	switch s {
	case "private", "":
		return imagev1.ImageVisibility_PRIVATE
	case "public":
		return imagev1.ImageVisibility_PUBLIC
	default:
		fatal("-visibility must be private or public, got %q", s)
		return imagev1.ImageVisibility_IMAGE_VISIBILITY_UNSPECIFIED
	}
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
