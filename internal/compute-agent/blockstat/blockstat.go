// Package blockstat reads a real block device's cumulative I/O counters
// from sysfs, for a Volume attachment's resolved DevicePath (see
// internal/compute-agent/volumeref) when that path is a block special
// file -- the ISCSI/NVME_OF case. An NFS-backed attachment resolves to a
// regular file instead, which has no equivalent per-file counter (NFS I/O
// never passes through the block layer); Stats reports that explicitly so
// callers can skip it rather than guessing from an error. See
// docs/architecture.md「払い出したリソース自身のメトリクス」.
package blockstat

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// sectorSize is the fixed 512-byte unit /sys/.../stat's sector counts use,
// per the kernel's Documentation/ABI/stable/sysfs-block -- independent of
// the device's actual logical block size.
const sectorSize = 512

// IO is one block device's cumulative I/O as of one read.
type IO struct {
	ReadBytes  int64
	WriteBytes int64
	ReadOps    int64
	WriteOps   int64
}

// root is the sysfs mount point. A package var (not a const) so tests can
// point it at a scratch directory instead of the real /sys.
var root = "/sys"

// Stats reads devicePath's cumulative I/O counters. ok is false (with a nil
// error) when devicePath is not a block special file -- the expected,
// non-error case for an NFS-backed Volume attachment.
func Stats(devicePath string) (io IO, ok bool, err error) {
	fi, err := os.Stat(devicePath)
	if err != nil {
		return IO{}, false, fmt.Errorf("blockstat: stat %s: %w", devicePath, err)
	}
	st, isStatT := fi.Sys().(*syscall.Stat_t)
	if !isStatT || fi.Mode()&os.ModeDevice == 0 || fi.Mode()&os.ModeCharDevice != 0 {
		return IO{}, false, nil
	}

	major := unix.Major(uint64(st.Rdev))
	minor := unix.Minor(uint64(st.Rdev))
	statPath := filepath.Join(root, "dev", "block", fmt.Sprintf("%d:%d", major, minor), "stat")
	f, err := os.Open(statPath)
	if err != nil {
		return IO{}, false, fmt.Errorf("blockstat: open %s: %w", statPath, err)
	}
	defer f.Close()

	io, err = parseStat(f)
	if err != nil {
		return IO{}, false, fmt.Errorf("blockstat: parse %s: %w", statPath, err)
	}
	return io, true, nil
}

// parseStat reads the whitespace-separated fields of a block device's
// sysfs "stat" file. Only the first 7 fields are documented as always
// present (reads completed/merged/sectors/ms, writes completed/merged/
// sectors); later kernels append more (in-flight, io-ms, discard stats),
// which this ignores.
func parseStat(f *os.File) (IO, error) {
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return IO{}, err
		}
		return IO{}, fmt.Errorf("empty stat file")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 7 {
		return IO{}, fmt.Errorf("expected at least 7 fields, got %d", len(fields))
	}
	readOps, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return IO{}, fmt.Errorf("parse reads completed: %w", err)
	}
	readSectors, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return IO{}, fmt.Errorf("parse sectors read: %w", err)
	}
	writeOps, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return IO{}, fmt.Errorf("parse writes completed: %w", err)
	}
	writeSectors, err := strconv.ParseInt(fields[6], 10, 64)
	if err != nil {
		return IO{}, fmt.Errorf("parse sectors written: %w", err)
	}
	return IO{
		ReadBytes:  readSectors * sectorSize,
		WriteBytes: writeSectors * sectorSize,
		ReadOps:    readOps,
		WriteOps:   writeOps,
	}, nil
}
