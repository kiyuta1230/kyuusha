package chvmm

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/chapi"
)

func TestHotplugMaxVCPU(t *testing.T) {
	cases := []struct {
		boot int32
		want int32
	}{
		{boot: 1, want: 2},
		{boot: 2, want: 4},
		{boot: 20, want: 32}, // 2x would be 40, capped at the absolute ceiling
		{boot: 32, want: 32}, // already at the ceiling
	}
	for _, c := range cases {
		if got := hotplugMaxVCPU(c.boot); got != c.want {
			t.Errorf("hotplugMaxVCPU(%d) = %d, want %d", c.boot, got, c.want)
		}
	}
}

func TestHotplugMemoryCeilingMB(t *testing.T) {
	cases := []struct {
		boot int64
		want int64
	}{
		{boot: 1024, want: 2048},
		{boot: 200000, want: 262144}, // 2x would be 400000, capped at the absolute ceiling
		{boot: 262144, want: 262144},
	}
	for _, c := range cases {
		if got := hotplugMemoryCeilingMB(c.boot); got != c.want {
			t.Errorf("hotplugMemoryCeilingMB(%d) = %d, want %d", c.boot, got, c.want)
		}
	}
}

// newTestManagerWithSocket sets up a Manager that believes vmID is
// currently running (so client(vmID) succeeds) and has a fake
// cloud-hypervisor api-socket server listening at the exact path
// apiSocketPath(vmID) computes.
func newTestManagerWithSocket(t *testing.T, vmID string, handler http.HandlerFunc) *Manager {
	t.Helper()
	runDir := t.TempDir()
	m := &Manager{RunDir: runDir, running: map[string]*runningVM{vmID: {pid: os.Getpid()}}}

	sockDir := filepath.Join(runDir, vmID)
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sockPath := m.apiSocketPath(vmID)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return m
}

func TestManager_LiveResize(t *testing.T) {
	var gotBody chapi.VmResize
	m := newTestManagerWithSocket(t, "vm1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := m.LiveResize(context.Background(), "vm1", 4, 2048); err != nil {
		t.Fatalf("LiveResize: %v", err)
	}
	if gotBody.DesiredVcpus != 4 || gotBody.DesiredRAM != 2048*1024*1024 {
		t.Errorf("request body = %+v", gotBody)
	}
}

func TestManager_LiveResize_UnknownVM(t *testing.T) {
	m := &Manager{RunDir: t.TempDir()}
	if err := m.LiveResize(context.Background(), "does-not-exist", 4, 2048); err == nil {
		t.Fatal("expected an error for a vm_id this Manager isn't running")
	}
}

func TestManager_LiveAddDisk(t *testing.T) {
	var gotBody chapi.DiskConfig
	m := newTestManagerWithSocket(t, "vm1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(chapi.PciDeviceInfo{ID: "volattach-vm1-vol1", Bdf: "0000:00:05.0"})
	})

	if err := m.LiveAddDisk(context.Background(), "vm1", "volattach-vm1-vol1", "/dev/sdb"); err != nil {
		t.Fatalf("LiveAddDisk: %v", err)
	}
	if gotBody.Path != "/dev/sdb" || gotBody.ID != "volattach-vm1-vol1" {
		t.Errorf("request body = %+v", gotBody)
	}
}

func TestManager_LiveRemoveDevice(t *testing.T) {
	m := newTestManagerWithSocket(t, "vm1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := m.LiveRemoveDevice(context.Background(), "vm1", "volattach-vm1-vol1"); err != nil {
		t.Fatalf("LiveRemoveDevice: %v", err)
	}
}

func TestManager_LiveRemoveDevice_AlreadyGoneIsSuccess(t *testing.T) {
	m := newTestManagerWithSocket(t, "vm1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("device not found"))
	})
	if err := m.LiveRemoveDevice(context.Background(), "vm1", "volattach-vm1-vol1"); err != nil {
		t.Fatalf("LiveRemoveDevice should tolerate an already-removed device, got: %v", err)
	}
}
