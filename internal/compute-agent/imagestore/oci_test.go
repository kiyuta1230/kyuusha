package imagestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// newTestOCIRegistry starts a minimal, single-artifact OCI Distribution
// server: enough of the spec (GET /v2/, GET manifest by tag, GET blob by
// digest) for oras-go/v2's client to resolve repo:tag and fetch its one
// layer -- real registries (registry:2, ghcr.io, etc.) speak the same
// protocol, so this stands in for one without needing Docker.
func newTestOCIRegistry(t *testing.T, repo, tag string, blob []byte) *httptest.Server {
	t.Helper()

	blobSum := sha256.Sum256(blob)
	blobDigest := digest.Digest("sha256:" + hex.EncodeToString(blobSum[:]))
	emptyConfigDigest := digest.Digest("sha256:" + hex.EncodeToString(sha256.New().Sum(nil)))

	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: emptyConfigDigest, Size: 0},
		Layers: []ocispec.Descriptor{
			{MediaType: "application/vnd.kyuusha.test.blob.v1", Digest: blobDigest, Size: int64(len(blob))},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestSum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(manifestSum[:])

	manifestPath := fmt.Sprintf("/v2/%s/manifests/%s", repo, tag)
	blobPath := fmt.Sprintf("/v2/%s/blobs/%s", repo, blobDigest.String())

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		case manifestPath:
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(manifestBytes)
		case blobPath:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", blobDigest.String())
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchOCIBlobResolvesTagAndFetchesLayer(t *testing.T) {
	blob := []byte("this is the kernel or rootfs blob content")
	srv := newTestOCIRegistry(t, "kyuusha/test-kernel", "v1", blob)
	host := strings.TrimPrefix(srv.URL, "http://")

	rc, err := fetchOCIBlob(context.Background(), host+"/kyuusha/test-kernel:v1", true)
	if err != nil {
		t.Fatalf("fetchOCIBlob: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != string(blob) {
		t.Fatalf("blob content = %q, want %q", got, blob)
	}
}

func TestFetchOCIBlobRejectsMissingReference(t *testing.T) {
	_, err := fetchOCIBlob(context.Background(), "registry.example.com/kyuusha/no-tag", true)
	if err == nil {
		t.Fatal("expected error for a reference with no tag/digest, got nil")
	}
}

func TestEnsureCachedFetchesViaOCIScheme(t *testing.T) {
	blob := []byte("rootfs bytes served over oci+http")
	blobSum := sha256.Sum256(blob)
	wantDigest := "sha256:" + hex.EncodeToString(blobSum[:])

	srv := newTestOCIRegistry(t, "kyuusha/test-rootfs", "v1", blob)
	host := strings.TrimPrefix(srv.URL, "http://")

	s := &Store{Dir: t.TempDir()}
	path, err := s.EnsureCached(context.Background(), "oci+http://"+host+"/kyuusha/test-rootfs:v1", wantDigest)
	if err != nil {
		t.Fatalf("EnsureCached: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached file: %v", err)
	}
	if string(got) != string(blob) {
		t.Fatalf("cached content = %q, want %q", got, blob)
	}
}

func TestEnsureCachedRejectsOCIDigestMismatch(t *testing.T) {
	blob := []byte("some content")
	srv := newTestOCIRegistry(t, "kyuusha/test-mismatch", "v1", blob)
	host := strings.TrimPrefix(srv.URL, "http://")

	s := &Store{Dir: t.TempDir()}
	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	_, err := s.EnsureCached(context.Background(), "oci+http://"+host+"/kyuusha/test-mismatch:v1", wrongDigest)
	if err == nil {
		t.Fatal("expected digest mismatch error, got nil")
	}
}

// newPushableTestOCIRegistry stubs the OCI Distribution API surface
// PushOCIBlob actually exercises: blob upload (POST to start a session,
// monolithic PUT to complete it), manifest push-by-digest (PackManifest),
// manifest fetch-by-digest (Tag reads back what PackManifest just pushed
// before re-pushing it under the tag), and manifest push-by-tag (Tag). A
// real registry (registry:2, ghcr.io, etc.) speaks the same protocol; this
// stands in for one without needing Docker -- same reasoning as
// newTestOCIRegistry above, extended to also accept pushes. Deliberately
// doesn't implement the OCI 1.1 Referrers API: oras-go/v2 only probes it
// for a manifest with a `subject` field, which PushOCIBlob's artifacts
// never set, so it's simply never called.
func newPushableTestOCIRegistry(t *testing.T, repo string) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	blobs := map[string][]byte{}
	manifests := map[string][]byte{} // keyed by both tag and digest string
	uploadsPath := "/v2/" + repo + "/blobs/uploads/"
	blobsPrefix := "/v2/" + repo + "/blobs/"
	manifestsPrefix := "/v2/" + repo + "/manifests/"

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))

		case r.Method == http.MethodPost && r.URL.Path == uploadsPath:
			// Monolithic upload only (see oras-go/v2's
			// blobStore.completePushAfterInitialPost): this session id is
			// never actually looked up, only round-tripped in the Location
			// header the client PUTs back to.
			w.Header().Set("Location", uploadsPath+"session-1")
			w.WriteHeader(http.StatusAccepted)

		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, uploadsPath):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			dgst := r.URL.Query().Get("digest")
			mu.Lock()
			blobs[dgst] = body
			mu.Unlock()
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, blobsPrefix):
			dgst := strings.TrimPrefix(r.URL.Path, blobsPrefix)
			mu.Lock()
			body, ok := blobs[dgst]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", dgst)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)

		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, manifestsPrefix):
			ref := strings.TrimPrefix(r.URL.Path, manifestsPrefix)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			sum := sha256.Sum256(body)
			dgst := "sha256:" + hex.EncodeToString(sum[:])
			mu.Lock()
			manifests[ref] = body
			manifests[dgst] = body
			mu.Unlock()
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, manifestsPrefix):
			ref := strings.TrimPrefix(r.URL.Path, manifestsPrefix)
			mu.Lock()
			body, ok := manifests[ref]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			sum := sha256.Sum256(body)
			dgst := "sha256:" + hex.EncodeToString(sum[:])
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", dgst)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)

		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestPushOCIBlobStreamsFileRoundTrip exercises PushOCIBlob for real against
// newPushableTestOCIRegistry, then fetches the artifact back via
// fetchOCIBlob -- verifying the streaming digest-then-push rewrite (see
// PushOCIBlob's doc comment: no more os.ReadFile into a []byte) still
// produces a correct, fetchable artifact. Uses a file bigger than typical
// in-memory HTTP client read buffers so a broken Seek/rewind (reading
// stale or partial content on the second, push-side pass) would show up
// as a content mismatch rather than passing by accident on a tiny file.
func TestPushOCIBlobStreamsFileRoundTrip(t *testing.T) {
	content := bytes.Repeat([]byte("kyuusha-root-disk-streaming-test-content-"), 100_000) // ~4.2MB
	path := filepath.Join(t.TempDir(), "root-disk.img")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	srv := newPushableTestOCIRegistry(t, "kyuusha-migration/vm-test")
	host := strings.TrimPrefix(srv.URL, "http://")

	url, gotDigest, err := PushOCIBlob(context.Background(), host, "", "kyuusha-migration/vm-test", "migrate-1", true, path)
	if err != nil {
		t.Fatalf("PushOCIBlob: %v", err)
	}

	wantSum := sha256.Sum256(content)
	wantDigest := "sha256:" + hex.EncodeToString(wantSum[:])
	if gotDigest != wantDigest {
		t.Fatalf("digest = %q, want %q", gotDigest, wantDigest)
	}
	wantURL := "oci+http://" + host + "/kyuusha-migration/vm-test:migrate-1"
	if url != wantURL {
		t.Fatalf("url = %q, want %q", url, wantURL)
	}

	ref := strings.TrimPrefix(url, "oci+http://")
	rc, err := fetchOCIBlob(context.Background(), ref, true)
	if err != nil {
		t.Fatalf("fetchOCIBlob: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read fetched blob: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("fetched content (%d bytes) does not match pushed content (%d bytes)", len(got), len(content))
	}
}
