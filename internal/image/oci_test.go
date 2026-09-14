package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// newTestOCIRegistry is service_test.go's httptest OCI counterpart to
// internal/compute-agent/imagestore's identical helper -- kept separate
// (not shared across packages) since image only ever needs to resolve a
// manifest, never fetch a blob, so this omits the blob endpoint entirely.
func newTestOCIRegistry(t *testing.T, repo, tag string) *httptest.Server {
	t.Helper()
	manifest := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])
	manifestPath := fmt.Sprintf("/v2/%s/manifests/%s", repo, tag)

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
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestIsOCIURL(t *testing.T) {
	cases := map[string]bool{
		"https://example.com/vmlinux":              false,
		"http://example.com/vmlinux":               false,
		"oci://registry.example.com/repo:tag":      true,
		"oci+http://registry.example.com/repo:tag": true,
	}
	for url, want := range cases {
		if got := isOCIURL(url); got != want {
			t.Errorf("isOCIURL(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestOCIReachableResolvesExistingTag(t *testing.T) {
	srv := newTestOCIRegistry(t, "kyuusha/kernel", "v1")
	host := strings.TrimPrefix(srv.URL, "http://")

	if err := ociReachable(context.Background(), "oci+http://"+host+"/kyuusha/kernel:v1"); err != nil {
		t.Fatalf("ociReachable: %v", err)
	}
}

func TestOCIReachableFailsForMissingTag(t *testing.T) {
	srv := newTestOCIRegistry(t, "kyuusha/kernel", "v1")
	host := strings.TrimPrefix(srv.URL, "http://")

	if err := ociReachable(context.Background(), "oci+http://"+host+"/kyuusha/kernel:does-not-exist"); err == nil {
		t.Fatal("expected error resolving a nonexistent tag, got nil")
	}
}

func TestOCIReachableFailsForMissingReference(t *testing.T) {
	if err := ociReachable(context.Background(), "oci://registry.example.com/kyuusha/no-tag"); err == nil {
		t.Fatal("expected error for a reference with no tag/digest, got nil")
	}
}
