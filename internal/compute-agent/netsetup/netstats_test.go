package netsetup

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func withScratchSysfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := sysfsRoot
	sysfsRoot = dir
	t.Cleanup(func() { sysfsRoot = old })
	return dir
}

func writeTapStats(t *testing.T, sysfsDir, tap string, rxBytes, txBytes int64) {
	t.Helper()
	dir := filepath.Join(sysfsDir, "class", "net", tap, "statistics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, v := range map[string]int64{"rx_bytes": rxBytes, "tx_bytes": txBytes} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strconv.FormatInt(v, 10)+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestStats(t *testing.T) {
	dir := withScratchSysfs(t)
	writeTapStats(t, dir, "tapabc123", 1024, 2048)

	got, err := Stats("tapabc123")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := IOStats{RxBytes: 1024, TxBytes: 2048}
	if got != want {
		t.Errorf("Stats() = %+v, want %+v", got, want)
	}
}

func TestStats_MissingTap(t *testing.T) {
	withScratchSysfs(t)
	if _, err := Stats("tapnonexistent"); err == nil {
		t.Error("Stats(nonexistent tap): got nil error, want one")
	}
}
