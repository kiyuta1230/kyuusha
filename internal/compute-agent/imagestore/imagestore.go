// Package imagestore is the local, digest-addressed cache compute-agent's
// VMM drivers (fcvmm, chvmm) share for Image artifacts (the kernel/rootfs
// blobs an ImageArtifact{url, digest} references -- see
// docs/specs/image.md). One Store is constructed in cmd/compute-agent/
// main.go and handed to both drivers, replacing what used to be two
// independent, URL-hash-keyed, digest-unverified caches (fcvmm's fc-cache,
// chvmm's ch-cache) that never deduplicated against each other.
//
// This is a small purpose-built package, not an import of containerd's own
// content/snapshotter packages -- see docs/architecture.md「イメージの
// ローカル管理: containerdのcontent store/snapshotterへの移行検討」for why:
// containerd's local content store drags in a heavy, largely irrelevant
// transitive dependency graph (an old pinned grpc version, Windows-only
// hcsshim, cgroups, etc.) when embedded outside a running containerd
// daemon, and its snapshotter abstraction targets unpacking OCI layer
// *directory trees*, not cloning a single raw disk image file, which is
// what an Image.spec.rootfs actually is (see CloneFile).
package imagestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Store is a digest-addressed local cache rooted at Dir. Safe for
// concurrent use by multiple goroutines (fcvmm and chvmm both call
// EnsureCached from their own Boot, which can run concurrently for
// different VMs).
type Store struct {
	// Dir is the cache root. Populated lazily by EnsureCached.
	Dir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// EnsureCached returns the local path to url's content, downloading it
// first if not already cached.
//
// When digest is non-empty (the "sha256:<hex>" form ImageArtifact.digest
// carries), the download is hashed while it streams and verified against
// digest before being made visible under its content-addressed path
// (Dir/blobs/sha256/<hex>) -- this is the first point anywhere in kyuusha
// that a declared digest is actually checked against real bytes
// (docs/specs/image.md previously documented this as "実際にartifactを
// 取得するハイパーバイザー側の責務（未実装）"). A mismatch is returned as an
// error and nothing is cached under that digest. Because the cache key is
// the verified digest itself, two Images pointing at different URLs for
// the same underlying bytes share one cached copy.
//
// digest == "" (an Image created before this existed -- image.Service's
// Create-time validation has never required kernel/rootfs to carry a
// digest, see docs/specs/image.md) falls back to the old behavior: keyed
// and deduped by a hash of the URL itself (Dir/blobs/legacy/<hex>),
// unverified. Every newly-fetched Image should carry a real digest; this
// path exists only so an older one still boots.
func (s *Store) EnsureCached(ctx context.Context, url, digest string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("imagestore: empty artifact URL")
	}
	key, dest, err := s.destPath(url, digest)
	if err != nil {
		return "", err
	}

	unlock := s.lockKey(key)
	defer unlock()

	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("imagestore: create cache dir: %w", err)
	}

	rc, err := s.open(ctx, url)
	if err != nil {
		return "", fmt.Errorf("imagestore: fetch %s: %w", url, err)
	}
	defer rc.Close()

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}

	var body io.Reader = rc
	h := sha256.New()
	if digest != "" {
		body = io.TeeReader(rc, h)
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("imagestore: write %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("imagestore: close %s: %w", dest, err)
	}

	if digest != "" {
		got := "sha256:" + hex.EncodeToString(h.Sum(nil))
		if got != digest {
			os.Remove(tmp)
			return "", fmt.Errorf("imagestore: digest mismatch for %s: declared %s, got %s", url, digest, got)
		}
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("imagestore: finalize %s: %w", dest, err)
	}
	return dest, nil
}

// open dispatches url to whichever transport its scheme names and returns a
// stream of the artifact's raw bytes -- EnsureCached treats the result
// identically regardless of which one served it (hash while streaming,
// verify against digest, commit to the content-addressed path). See
// docs/architecture.md「Track 2実装方針」for why the scheme prefix (not a
// new proto field) is how an oras-go/v2-backed OCI registry reference is
// told apart from a plain HTTP(S) URL.
//
//   - "https://", "http://": unchanged from before Track 2 -- a plain GET
//   - "oci://<registry>/<repo>:<tag-or-digest>": OCI registry over HTTPS
//   - "oci+http://<registry>/<repo>:<tag-or-digest>": OCI registry over
//     plain HTTP -- for a registry with no TLS in front of it (playground's
//     `registry:2` service; see docs/architecture.md's addendum). There is
//     no real-world use for pulling a genuine public registry unencrypted,
//     so this scheme exists purely for that case
func (s *Store) open(ctx context.Context, url string) (io.ReadCloser, error) {
	switch {
	case strings.HasPrefix(url, "oci://"):
		return fetchOCIBlob(ctx, strings.TrimPrefix(url, "oci://"), false)
	case strings.HasPrefix(url, "oci+http://"):
		return fetchOCIBlob(ctx, strings.TrimPrefix(url, "oci+http://"), true)
	default:
		return fetchHTTP(ctx, url)
	}
}

// fetchHTTP is EnsureCached's original (pre-Track-2) transport: a plain GET,
// no registry protocol involved.
func fetchHTTP(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return resp.Body, nil
}

// destPath computes EnsureCached's cache key and on-disk path for url/
// digest. digest, when present, must be "sha256:<64 hex chars>" -- the
// only algorithm ImageArtifact.digest is documented to carry (see
// docs/specs/image.md); anything else is rejected rather than silently
// falling back to the legacy URL-keyed path, since that would let a typo'd
// digest field silently skip verification.
func (s *Store) destPath(url, digest string) (key, path string, err error) {
	if digest == "" {
		sum := sha256.Sum256([]byte(url))
		h := hex.EncodeToString(sum[:])
		return "legacy:" + h, filepath.Join(s.Dir, "blobs", "legacy", h), nil
	}
	algo, hexDigest, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" || len(hexDigest) != 64 {
		return "", "", fmt.Errorf("imagestore: unsupported digest %q (want sha256:<64 hex chars>)", digest)
	}
	return digest, filepath.Join(s.Dir, "blobs", "sha256", hexDigest), nil
}

// lockKey serializes EnsureCached calls that share the same cache key
// (concurrent Boot calls for different VMs referencing the same Image),
// without blocking unrelated keys the way the single global mutex each of
// fcvmm/chvmm used to hold did. Per-key *sync.Mutex entries are never
// removed -- an unbounded but slowly-growing map is an acceptable trade at
// this system's target scale (a few thousand distinct Images at most, see
// docs/architecture.md's scale assumptions), same simplification this
// codebase makes elsewhere.
func (s *Store) lockKey(key string) (unlock func()) {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()

	l.Lock()
	return l.Unlock
}

// CloneFile makes dst a copy-on-write clone of src's current contents when
// the underlying filesystem supports it (Btrfs, XFS with reflink=1,
// overlayfs backed by either -- via the FICLONE ioctl), falling back to a
// plain byte-for-byte copy otherwise (e.g. ext4, tmpfs, or src/dst on
// different filesystems, which FICLONE always rejects). dst is created if
// missing and truncated if it already exists.
//
// Every VM's writable rootfs copy (fcvmm's rootfs.ext4 inside its jail,
// chvmm's rootfs.raw) goes through this instead of a snapshotter-managed
// overlay mount: an Image.spec.rootfs is a single raw disk image file (see
// docker/Dockerfile's image-assets stage, which builds it via
// `mkfs.ext4 -d`), not an OCI layer directory tree, so per-VM CoW here
// means cloning one file, not overlay-mounting a union of many -- the
// problem containerd's snapshotters are actually built to solve.
func CloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err == nil {
		return nil
	}

	// FICLONE unsupported on this filesystem/pair -- fall back to a plain
	// copy. A failed clone attempt can leave dst partially written on some
	// filesystems, so start over from a known-empty file.
	if err := out.Truncate(0); err != nil {
		return err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return nil
}
