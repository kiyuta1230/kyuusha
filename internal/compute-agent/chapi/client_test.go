package chapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// newFakeServer starts an httptest.Server listening on a Unix socket under
// t.TempDir() (httptest has no native Unix-socket constructor, so this
// wires one up by hand: create the listener ourselves, then hand it to an
// unstarted server). Returns the Client pointed at it and the socket path
// for callers that want to assert against requests received.
func newFakeServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return New(sockPath)
}

func TestClient_Resize(t *testing.T) {
	var gotPath string
	var gotBody VmResize
	c := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.Resize(context.Background(), VmResize{DesiredVcpus: 4, DesiredRAM: 2 << 30}); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if gotPath != "/api/v1/vm.resize" {
		t.Errorf("path = %q, want /api/v1/vm.resize", gotPath)
	}
	if gotBody.DesiredVcpus != 4 || gotBody.DesiredRAM != 2<<30 {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestClient_AddDisk(t *testing.T) {
	var gotBody DiskConfig
	c := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/vm.add-disk" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(PciDeviceInfo{ID: "volattach-vm1-vol1", Bdf: "0000:00:05.0"})
	})

	info, err := c.AddDisk(context.Background(), DiskConfig{Path: "/dev/sdb", ID: "volattach-vm1-vol1"})
	if err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	if gotBody.Path != "/dev/sdb" || gotBody.ID != "volattach-vm1-vol1" {
		t.Errorf("body = %+v", gotBody)
	}
	if info.ID != "volattach-vm1-vol1" || info.Bdf != "0000:00:05.0" {
		t.Errorf("response = %+v", info)
	}
}

func TestClient_RemoveDevice(t *testing.T) {
	var gotBody VmRemoveDevice
	c := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/vm.remove-device" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.RemoveDevice(context.Background(), "volattach-vm1-vol1"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if gotBody.ID != "volattach-vm1-vol1" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestClient_NonSuccessStatus(t *testing.T) {
	c := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("device not found"))
	})

	err := c.RemoveDevice(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}
