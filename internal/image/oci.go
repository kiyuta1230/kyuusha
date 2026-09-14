package image

import (
	"context"
	"fmt"
	"strings"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
)

// isOCIURL reports whether url names an OCI registry reference (Track 2)
// rather than a plain HTTP(S) URL -- see docs/architecture.md「Track 2実装
// 方針」.
func isOCIURL(url string) bool {
	return strings.HasPrefix(url, "oci://") || strings.HasPrefix(url, "oci+http://")
}

// ociReachable checks that url's tag/digest actually resolves against the
// registry, without downloading the manifest or blob body -- the OCI
// equivalent of headCheck's plain HTTP HEAD. This is intentionally the same
// check internal/compute-agent/imagestore will later have to redo in full
// (resolve + fetch) at VM boot time: unlike a Volume's existence
// (checked once, by StorageConnection, see docs/specs/volume.md), an
// Image's reachability here is a best-effort Create-time signal, not a
// standing guarantee -- the registry entry could still disappear or move
// before any VM actually boots from it.
func ociReachable(ctx context.Context, url string) error {
	var ref string
	var plainHTTP bool
	switch {
	case strings.HasPrefix(url, "oci://"):
		ref = strings.TrimPrefix(url, "oci://")
	case strings.HasPrefix(url, "oci+http://"):
		ref = strings.TrimPrefix(url, "oci+http://")
		plainHTTP = true
	default:
		return fmt.Errorf("not an OCI reference: %s", url)
	}

	parsed, err := registry.ParseReference(ref)
	if err != nil {
		return fmt.Errorf("parse OCI reference: %w", err)
	}
	if err := parsed.ValidateReference(); err != nil {
		return fmt.Errorf("OCI reference has no tag or digest: %w", err)
	}

	repo, err := remote.NewRepository(parsed.Registry + "/" + parsed.Repository)
	if err != nil {
		return fmt.Errorf("open OCI repository: %w", err)
	}
	repo.PlainHTTP = plainHTTP

	if _, err := oras.Resolve(ctx, repo, parsed.ReferenceOrDefault(), oras.DefaultResolveOptions); err != nil {
		return fmt.Errorf("resolve OCI manifest: %w", err)
	}
	return nil
}
