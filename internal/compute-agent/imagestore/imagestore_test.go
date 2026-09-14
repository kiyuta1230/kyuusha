package imagestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestEnsureCachedVerifiesDigest(t *testing.T) {
	content := []byte("kernel bytes")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	s := &Store{Dir: t.TempDir()}
	path, err := s.EnsureCached(context.Background(), srv.URL, digest)
	if err != nil {
		t.Fatalf("EnsureCached: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("cached content = %q, want %q", got, content)
	}
	wantPath := filepath.Join(s.Dir, "blobs", "sha256", hex.EncodeToString(sum[:]))
	if path != wantPath {
		t.Fatalf("cached path = %q, want %q (content-addressed by digest)", path, wantPath)
	}
}

func TestEnsureCachedRejectsDigestMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("actual bytes"))
	}))
	defer srv.Close()

	s := &Store{Dir: t.TempDir()}
	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	_, err := s.EnsureCached(context.Background(), srv.URL, wrongDigest)
	if err == nil {
		t.Fatal("expected digest mismatch error, got nil")
	}

	// Nothing should be left behind under that (wrong) digest's path, and
	// no stray .tmp file either.
	blobDir := filepath.Join(s.Dir, "blobs", "sha256")
	entries, _ := os.ReadDir(blobDir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		t.Fatalf("leftover tmp file after digest mismatch: %s", e.Name())
	}
}

func TestEnsureCachedRejectsUnsupportedDigestAlgo(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	_, err := s.EnsureCached(context.Background(), "http://example.invalid/x", "md5:deadbeef")
	if err == nil {
		t.Fatal("expected error for unsupported digest algorithm, got nil")
	}
}

func TestEnsureCachedFallsBackWithoutDigest(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("legacy bytes"))
	}))
	defer srv.Close()

	s := &Store{Dir: t.TempDir()}
	path1, err := s.EnsureCached(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("EnsureCached: %v", err)
	}
	path2, err := s.EnsureCached(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("EnsureCached (second call): %v", err)
	}
	if path1 != path2 {
		t.Fatalf("expected same cached path for repeated legacy fetch, got %q and %q", path1, path2)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected exactly 1 HTTP fetch (second call should hit cache), got %d", hits)
	}
}

func TestEnsureCachedDedupesConcurrentFetches(t *testing.T) {
	content := []byte("concurrent bytes")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	var hits int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release // hold the first request open so concurrent callers actually overlap
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	s := &Store{Dir: t.TempDir()}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.EnsureCached(context.Background(), srv.URL, digest)
		}(i)
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: EnsureCached: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 HTTP fetch across %d concurrent callers for the same digest, got %d", n, got)
	}
}

func TestCloneFileCopiesContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	want := []byte("rootfs master copy")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := CloneFile(src, dst); err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("dst content = %q, want %q", got, want)
	}

	// Overwriting src afterward must not change dst -- whether CloneFile
	// took the reflink path or the plain-copy fallback, dst must be an
	// independent copy, not a hard link/alias.
	if err := os.WriteFile(src, []byte("mutated"), 0o644); err != nil {
		t.Fatalf("rewrite src: %v", err)
	}
	got, err = os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst after src mutation: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("dst changed after src was rewritten (not independent): got %q, want %q", got, want)
	}
}

func TestCloneFileOverwritesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("much longer stale content to overwrite"), 0o644); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	if err := CloneFile(src, dst); err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new content" {
		t.Fatalf("dst content = %q, want %q (stale tail not truncated)", got, "new content")
	}
}
