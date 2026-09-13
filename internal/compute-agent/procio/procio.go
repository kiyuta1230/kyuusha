// Package procio reads a process's cumulative disk I/O byte counts from
// /proc/<pid>/io. Unlike cgroup v2's io controller (internal/compute-agent/
// cgroup), which only accounts for block-layer I/O, this is task-level
// accounting hooked into the generic VFS read/write path -- it counts a
// VM's I/O against an NFS-backed Volume (a regular file) just as well as
// one backed by a real block device, making it the one disk metric that
// works regardless of block-storage backend. See
// docs/architecture.md「払い出したリソース自身のメトリクス」.
package procio

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DiskIO is a VM's cumulative disk I/O as of one read.
type DiskIO struct {
	ReadBytes  int64
	WriteBytes int64
}

// root is the procfs mount point. A package var (not a const) so tests can
// point it at a scratch directory instead of the real /proc.
var root = "/proc"

// Read parses /proc/<pid>/io for pid's read_bytes/write_bytes fields --
// the "actual" bytes fetched from/sent to storage, per proc(5), as opposed
// to rchar/wchar which also count bytes served from cache.
func Read(pid int) (DiskIO, error) {
	f, err := os.Open(filepath.Join(root, strconv.Itoa(pid), "io"))
	if err != nil {
		return DiskIO{}, fmt.Errorf("procio: open: %w", err)
	}
	defer f.Close()

	var io DiskIO
	var sawRead, sawWrite bool
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "read_bytes":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return DiskIO{}, fmt.Errorf("procio: parse read_bytes %q: %w", value, err)
			}
			io.ReadBytes = n
			sawRead = true
		case "write_bytes":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return DiskIO{}, fmt.Errorf("procio: parse write_bytes %q: %w", value, err)
			}
			io.WriteBytes = n
			sawWrite = true
		}
	}
	if err := scanner.Err(); err != nil {
		return DiskIO{}, fmt.Errorf("procio: read: %w", err)
	}
	if !sawRead || !sawWrite {
		return DiskIO{}, fmt.Errorf("procio: read_bytes/write_bytes not found in /proc/%d/io", pid)
	}
	return io, nil
}
