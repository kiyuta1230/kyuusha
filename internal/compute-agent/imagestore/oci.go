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

// PushOCIBlob pushes blob to registryAddr/repo:tag as a single-layer OCI
// artifact -- the exact same push shape cmd/kyuusha's `image build` uses
// (orasPushFile there, duplicated rather than shared: that's a CLI-only
// command this package can't import), so the result is byte-for-byte the
// same artifact shape fetchOCIBlob already knows how to pull back
// (manifest with exactly one layer). Used by Migrate(transfer_root_disk=
// true)'s push handler (agent.go) to publish a VM's current root disk;
// registryRef, if non-empty, is what gets embedded in the returned URL
// instead of registryAddr -- the same "push via one address, but the
// consuming side reaches the registry at a different address" split
// -registry/-registry-ref gives `kyuusha image build` (e.g. compute-agent
// containers resolve the registry via internal Docker DNS, this process
// may not). Loading the whole file into memory (like orasPushFile already
// does) is a known limitation for very large root disks -- see
// docs/open-questions.md.
func PushOCIBlob(ctx context.Context, registryAddr, registryRef, repo, tag string, plainHTTP bool, blob []byte) (url, digest string, err error) {
	repository, err := remote.NewRepository(registryAddr + "/" + repo)
	if err != nil {
		return "", "", fmt.Errorf("open OCI repository %s/%s: %w", registryAddr, repo, err)
	}
	repository.PlainHTTP = plainHTTP

	layerDesc, err := oras.PushBytes(ctx, repository, "application/octet-stream", blob)
	if err != nil {
		return "", "", fmt.Errorf("push OCI blob to %s/%s:%s: %w", registryAddr, repo, tag, err)
	}
	manifestDesc, err := oras.PackManifest(ctx, repository, oras.PackManifestVersion1_1, "application/vnd.kyuusha.rootdisk.v1", oras.PackManifestOptions{
		Layers: []ocispec.Descriptor{layerDesc},
	})
	if err != nil {
		return "", "", fmt.Errorf("pack OCI manifest for %s/%s:%s: %w", registryAddr, repo, tag, err)
	}
	if err := repository.Tag(ctx, manifestDesc, tag); err != nil {
		return "", "", fmt.Errorf("tag OCI manifest %s/%s:%s: %w", registryAddr, repo, tag, err)
	}

	scheme := "oci://"
	if plainHTTP {
		scheme = "oci+http://"
	}
	ref := registryRef
	if ref == "" {
		ref = registryAddr
	}
	return fmt.Sprintf("%s%s/%s:%s", scheme, ref, repo, tag), layerDesc.Digest.String(), nil
}

// DeleteOCIRef removes the manifest ref names ("<registry>/<repository>:
// <tag-or-digest>", the scheme already stripped by the caller) from its
// registry -- the cleanup counterpart to PushOCIBlob, used once a migrated
// VM is confirmed booted from a pushed root disk artifact (see
// reconciler.go's handleCreateResult). Best-effort by design at the call
// site: many registries disable manifest deletion by default (e.g. the
// stock Docker Registry image needs REGISTRY_STORAGE_DELETE_ENABLED=true),
// and a failure here only leaks registry storage, never correctness.
func DeleteOCIRef(ctx context.Context, ref string, plainHTTP bool) error {
	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("parse OCI reference %q: %w", ref, err)
	}
	if err := parsed.ValidateReference(); err != nil {
		return fmt.Errorf("OCI reference %q has no tag or digest: %w", ref, err)
	}

	repo, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		return fmt.Errorf("open OCI repository %s/%s: %w", parsed.Registry, parsed.Repository, err)
	}
	repo.PlainHTTP = plainHTTP

	desc, err := repo.Resolve(ctx, parsed.ReferenceOrDefault())
	if err != nil {
		return fmt.Errorf("resolve OCI manifest %s: %w", ref, err)
	}
	if err := repo.Manifests().Delete(ctx, desc); err != nil {
		return fmt.Errorf("delete OCI manifest %s: %w", ref, err)
	}
	return nil
}
