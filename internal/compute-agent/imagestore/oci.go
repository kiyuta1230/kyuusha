package imagestore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

// fetchOCIBlob resolves ref ("<registry>/<repository>:<tag-or-digest>", the
// scheme already stripped by open) against an OCI registry and returns a
// stream of the single artifact blob it names.
//
// kyuusha's kernel/rootfs/qcow2 artifacts are each pushed (see the intended
// `kyuusha image build` CLI tool, docs/architecture.md「Track 2実装方針」)
// as a single-layer OCI artifact -- one raw file, not an OCI *image* whose
// layers are unpacked into a container root filesystem. So resolving ref
// gives the artifact *manifest*, not the file itself: this fetches that
// manifest, takes its one layer's descriptor, and fetches that blob --
// which oras-go's Repository.Blobs().Fetch itself verifies against the
// layer descriptor's own digest as it streams (a corrupt or truncated
// transfer errors out here, independently of and before EnsureCached's own
// digest check against ImageArtifact.digest).
func fetchOCIBlob(ctx context.Context, ref string, plainHTTP bool) (io.ReadCloser, error) {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("parse OCI reference %q: %w", ref, err)
	}
	if err := parsed.ValidateReference(); err != nil {
		return nil, fmt.Errorf("OCI reference %q has no tag or digest: %w", ref, err)
	}

	repo, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		return nil, fmt.Errorf("open OCI repository %s/%s: %w", parsed.Registry, parsed.Repository, err)
	}
	repo.PlainHTTP = plainHTTP

	_, manifestRC, err := oras.Fetch(ctx, repo, parsed.ReferenceOrDefault(), oras.DefaultFetchOptions)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI manifest %s: %w", ref, err)
	}
	manifestBytes, err := io.ReadAll(manifestRC)
	manifestRC.Close()
	if err != nil {
		return nil, fmt.Errorf("read OCI manifest %s: %w", ref, err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode OCI manifest %s: %w", ref, err)
	}
	if len(manifest.Layers) != 1 {
		return nil, fmt.Errorf("OCI artifact %s has %d layers, kyuusha expects exactly 1 (one raw kernel/rootfs/disk blob per artifact)", ref, len(manifest.Layers))
	}

	blobRC, err := repo.Blobs().Fetch(ctx, manifest.Layers[0])
	if err != nil {
		return nil, fmt.Errorf("fetch OCI blob %s (%s): %w", ref, manifest.Layers[0].Digest, err)
	}
	return blobRC, nil
}
