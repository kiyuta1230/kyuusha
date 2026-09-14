package imagestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
