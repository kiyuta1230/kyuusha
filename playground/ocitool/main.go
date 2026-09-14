// Command ocitool seeds playground's OCI registry (see
// docs/architecture.md「Track 2実装方針」) with the same kernel/rootfs test
// assets image-assets already serves over plain HTTP, so a Track 2
// oci+http:// Image has something real to pull from in playground.
//
// This is playground-only scaffolding, not one of kyuusha's own binaries
// (see README.md's component table) -- a stand-in for the eventual
// `kyuusha image build` CLI tool (docs/specs/image.md's 未実装 list),
// scoped down to exactly what playground verification needs: push two
// already-built files as single-layer OCI artifacts and tag them, nothing
// more (no OCI-layer flattening from a Dockerfile, no image service Create
// call -- playground/scenario.sh or a manual `kyuusha image create` does
// that part against the oci+http:// reference this prints).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
)

func main() {
	registryAddr := flag.String("registry", "registry:5000", "OCI registry host:port (plain HTTP, see playground/docker-compose.yml's registry service)")
	assetsBaseURL := flag.String("assets", "http://image-assets", "image-assets base URL to fetch the source files from")
	flag.Parse()

	ctx := context.Background()
	if err := seed(ctx, *registryAddr, *assetsBaseURL+"/vmlinux", "kyuusha/vmlinux", "v1", "application/vnd.kyuusha.kernel.v1"); err != nil {
		log.Fatalf("ocitool: seed kernel: %v", err)
	}
	if err := seed(ctx, *registryAddr, *assetsBaseURL+"/rootfs.ext4", "kyuusha/rootfs", "v1", "application/vnd.kyuusha.rootfs.v1"); err != nil {
		log.Fatalf("ocitool: seed rootfs: %v", err)
	}
}

// seed fetches sourceURL's bytes and pushes them to registryAddr as a
// single-layer OCI artifact tagged repo:tag -- the same "one raw file per
// artifact" convention internal/compute-agent/imagestore's fetchOCIBlob
// expects (manifest.Layers must have exactly one entry).
func seed(ctx context.Context, registryAddr, sourceURL, repo, tag, artifactType string) error {
	resp, err := http.Get(sourceURL)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: unexpected status %s", sourceURL, resp.Status)
	}
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s: %w", sourceURL, err)
	}

	repository, err := remote.NewRepository(registryAddr + "/" + repo)
	if err != nil {
		return fmt.Errorf("open repository %s/%s: %w", registryAddr, repo, err)
	}
	repository.PlainHTTP = true

	layerDesc, err := oras.PushBytes(ctx, repository, "application/octet-stream", blob)
	if err != nil {
		return fmt.Errorf("push blob: %w", err)
	}
	manifestDesc, err := oras.PackManifest(ctx, repository, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{
		Layers: []ocispec.Descriptor{layerDesc},
	})
	if err != nil {
		return fmt.Errorf("pack manifest: %w", err)
	}
	if err := repository.Tag(ctx, manifestDesc, tag); err != nil {
		return fmt.Errorf("tag manifest: %w", err)
	}

	fmt.Printf("ocitool: pushed oci+http://%s/%s:%s (%d bytes, digest %s)\n", registryAddr, repo, tag, len(blob), layerDesc.Digest)
	return nil
}
