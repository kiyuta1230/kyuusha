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
	"time"
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

// cacheBlob is a small EnsureCached helper for the Sweep/Pin tests below:
// it fetches distinct content (so it gets its own digest-addressed path)
// and returns that path's key and byte size.
func cacheBlob(t *testing.T, s *Store, content []byte) (key, path string, size int64) {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()
	path, err := s.EnsureCached(context.Background(), srv.URL, digest)
	if err != nil {
		t.Fatalf("EnsureCached: %v", err)
	}
	key, err = s.Key(srv.URL, digest)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	return key, path, int64(len(content))
}

func TestSweepDisabledWhenMaxBytesNonPositive(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	_, path, _ := cacheBlob(t, s, []byte("some kernel bytes"))

	freed, err := s.Sweep(0)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0 (Sweep disabled)", freed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("blob was evicted despite Sweep being disabled: %v", err)
	}
}

func TestSweepNoopUnderThreshold(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	_, path, size := cacheBlob(t, s, []byte("small"))

	freed, err := s.Sweep(size * 10)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if freed != 0 {
		t.Fatalf("freed = %d, want 0 (under threshold)", freed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("blob evicted despite being under the size threshold: %v", err)
	}
}

func TestSweepEvictsLeastRecentlyUsedFirst(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	_, oldPath, oldSize := cacheBlob(t, s, []byte("oldest blob"))
	_, newPath, newSize := cacheBlob(t, s, []byte("newest blob"))

	now := time.Now()
	if err := os.Chtimes(oldPath, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("Chtimes old: %v", err)
	}
	if err := os.Chtimes(newPath, now, now); err != nil {
		t.Fatalf("Chtimes new: %v", err)
	}

	// Only room for one of the two -- the older one must go.
	freed, err := s.Sweep(newSize)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if freed != oldSize {
		t.Fatalf("freed = %d, want %d (the older blob's size)", freed, oldSize)
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Fatal("least-recently-used blob was not evicted")
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("most-recently-used blob was evicted: %v", err)
	}
}

func TestSweepSkipsPinnedEntries(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	pinnedKey, pinnedPath, _ := cacheBlob(t, s, []byte("pinned but stale blob"))
	_, freePath, freeSize := cacheBlob(t, s, []byte("unpinned, also stale"))

	now := time.Now()
	// Both look equally stale by mtime -- Pin, not recency, must be what
	// decides which one Sweep is willing to touch.
	_ = os.Chtimes(pinnedPath, now.Add(-time.Hour), now.Add(-time.Hour))
	_ = os.Chtimes(freePath, now.Add(-time.Hour), now.Add(-time.Hour))

	s.Pin(pinnedKey)
	defer s.Unpin(pinnedKey)

	// Ask Sweep to shrink to nothing -- with no pin at all this would evict
	// both.
	freed, err := s.Sweep(1)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if freed != freeSize {
		t.Fatalf("freed = %d, want %d (only the unpinned blob)", freed, freeSize)
	}
	if _, err := os.Stat(pinnedPath); err != nil {
		t.Fatalf("pinned blob was evicted: %v", err)
	}
	if _, err := os.Stat(freePath); err == nil {
		t.Fatal("unpinned stale blob was not evicted")
	}
}

func TestPinUnpinRoundTrip(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	const key = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

	if s.isPinned(key) {
		t.Fatal("key reported pinned before any Pin call")
	}
	s.Pin(key)
	s.Pin(key) // a second VM sharing the same Image also pins it
	if !s.isPinned(key) {
		t.Fatal("key not pinned after Pin")
	}
	s.Unpin(key)
	if !s.isPinned(key) {
		t.Fatal("key unpinned after only one of two Unpin calls")
	}
	s.Unpin(key)
	if s.isPinned(key) {
		t.Fatal("key still pinned after matching Unpin calls")
	}
	// Unpinning past zero must not go negative or panic.
	s.Unpin(key)
	if s.isPinned(key) {
		t.Fatal("key pinned after an extra, unbalanced Unpin")
	}
}

func TestEnsureCachedTouchesMtimeOnHit(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	_, path, _ := cacheBlob(t, s, []byte("touched on hit"))

	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	sum := sha256.Sum256([]byte("touched on hit"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	// EnsureCached needs a live URL to fall back to only if this is a
	// miss; it must be a hit here since path already exists, so the URL
	// itself is never dialed.
	if _, err := s.EnsureCached(context.Background(), "http://unused.invalid", digest); err != nil {
		t.Fatalf("EnsureCached (cache hit): %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().After(stale) {
		t.Fatalf("mtime not touched on cache hit: got %v, want after %v", info.ModTime(), stale)
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
