package blockstat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStats_RegularFileIsNotABlockDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume.img")
	if err := os.WriteFile(path, []byte("not a device"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	io, ok, err := Stats(path)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if ok {
		t.Errorf("Stats(regular file) ok = true, want false (%+v)", io)
	}
}

func TestStats_NonexistentPath(t *testing.T) {
	if _, _, err := Stats(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("Stats(nonexistent path): got nil error, want one")
	}
}

// parseStat is exercised directly (rather than through Stats) since
// constructing a real block special file needs CAP_MKNOD -- Stats' "is
// this actually a block device" branch is instead only exercised by real
// deployment against ISCSI/NVME_OF, same as this project's other
// not-yet-hardware-tested paths (PCI passthrough, DRBD).
func TestParseStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	// reads-completed reads-merged sectors-read ms-reading
	// writes-completed writes-merged sectors-written ms-writing
	if err := os.WriteFile(path, []byte("100 5 800 10 50 2 400 20\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	got, err := parseStat(f)
	if err != nil {
		t.Fatalf("parseStat: %v", err)
	}
	want := IO{ReadBytes: 800 * sectorSize, WriteBytes: 400 * sectorSize, ReadOps: 100, WriteOps: 50}
	if got != want {
		t.Errorf("parseStat() = %+v, want %+v", got, want)
	}
}

func TestParseStat_TooFewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte("1 2 3\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, err := parseStat(f); err == nil {
		t.Error("parseStat(3 fields): got nil error, want one")
	}
}
