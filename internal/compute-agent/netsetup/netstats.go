package netsetup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sysfsRoot is the sysfs mount point. A package var (not a const) so tests
// can point it at a scratch directory instead of the real /sys.
var sysfsRoot = "/sys"

// IOStats is one tap device's cumulative network I/O as of one read.
type IOStats struct {
	RxBytes int64
	TxBytes int64
}

// Stats reads a Wire'd tap device's cumulative rx/tx byte counters from
// /sys/class/net/<tap>/statistics -- the same interface Firecracker/
// cloud-hypervisor's virtio-net backend actually pushes packets through,
// so this counts real guest traffic regardless of driver_hint. See
// docs/architecture.md「払い出したリソース自身のメトリクス」.
func Stats(tapName string) (IOStats, error) {
	dir := filepath.Join(sysfsRoot, "class", "net", tapName, "statistics")
	rx, err := readCounter(filepath.Join(dir, "rx_bytes"))
	if err != nil {
		return IOStats{}, fmt.Errorf("netsetup: read rx_bytes: %w", err)
	}
	tx, err := readCounter(filepath.Join(dir, "tx_bytes"))
	if err != nil {
		return IOStats{}, fmt.Errorf("netsetup: read tx_bytes: %w", err)
	}
	return IOStats{RxBytes: rx, TxBytes: tx}, nil
}

func readCounter(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
