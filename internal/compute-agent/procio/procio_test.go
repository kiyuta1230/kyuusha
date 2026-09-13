package procio

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func withScratchRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := root
	root = dir
	t.Cleanup(func() { root = old })
	return dir
}

func writeProcIO(t *testing.T, dir string, pid int, content string) {
	t.Helper()
	pidDir := filepath.Join(dir, strconv.Itoa(pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "io"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

const sampleProcIO = `rchar: 123456
wchar: 654321
syscr: 10
syscw: 12
read_bytes: 4096
write_bytes: 8192
cancelled_write_bytes: 0
`

func TestRead(t *testing.T) {
	dir := withScratchRoot(t)
	writeProcIO(t, dir, 4242, sampleProcIO)

	got, err := Read(4242)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := DiskIO{ReadBytes: 4096, WriteBytes: 8192}
	if got != want {
		t.Errorf("Read() = %+v, want %+v", got, want)
	}
}

func TestRead_MissingProcess(t *testing.T) {
	withScratchRoot(t)
	if _, err := Read(99999); err == nil {
		t.Error("Read() for a nonexistent pid: got nil error, want one")
	}
}

func TestRead_MissingFields(t *testing.T) {
	dir := withScratchRoot(t)
	writeProcIO(t, dir, 55, "rchar: 1\nwchar: 2\n")

	if _, err := Read(55); err == nil {
		t.Error("Read() with no read_bytes/write_bytes lines: got nil error, want one")
	}
}
