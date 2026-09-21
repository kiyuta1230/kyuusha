// Package chapi is a minimal client for cloud-hypervisor's --api-socket
// HTTP API (docs/specs/cloud-hypervisor-boot.md「--api-socket」) -- covers
// only the three endpoints kyuusha's live-hotplug path needs (vm.resize,
// vm.add-disk, vm.remove-device). Deliberately hand-rolled against the
// stdlib rather than importing a generated OpenAPI client: the surface
// needed is tiny (three PUTs, no polling/streaming), and this keeps the
// "difficult distributed-systems problems only" bar for third-party
// dependencies (docs/architecture.md) from being crossed for what's a
// direct application of net/http against a local Unix socket.
package chapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Client talks to one VM's cloud-hypervisor --api-socket. Callers construct
// one per hotplug request (see internal/compute-agent/chvmm/hotplug.go) --
// it holds no long-lived connection of its own beyond the underlying
// http.Client's normal connection pooling, so there is nothing to close.
type Client struct {
	httpClient *http.Client
}

// New returns a Client that dials socketPath (a Unix domain socket) for
// every request, ignoring whatever host/URL is otherwise passed to it --
// same "the URL's host is a dummy, only the path matters" convention every
// Unix-socket HTTP client (Docker's, containerd's) uses, since net/http
// requires a URL with *some* host even though DialContext overrides where
// it actually connects.
func New(socketPath string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// baseURL's host is never actually dialed (see New) -- only ever appended
// to, to build each endpoint's path.
const baseURL = "http://localhost/api/v1/"

// VmResize is the request body for PUT vm.resize. Zero-value fields are
// omitted (cloud-hypervisor's own convention: an absent field means "leave
// this alone", not "set to zero") -- LiveResize only ever needs to set
// DesiredVcpus/DesiredRAM, never DesiredBalloon.
type VmResize struct {
	DesiredVcpus   int32 `json:"desired_vcpus,omitempty"`
	DesiredRAM     int64 `json:"desired_ram,omitempty"`     // bytes
	DesiredBalloon int64 `json:"desired_balloon,omitempty"` // bytes
}

// DiskConfig is the request body for PUT vm.add-disk. Only the fields
// kyuusha's live-attach path sets are listed here; every other field
// cloud-hypervisor's DiskConfig accepts is left at its documented default
// by omission.
type DiskConfig struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
	Direct   bool   `json:"direct"`
	// ID is caller-chosen (kyuusha always sets it to the owning
	// VolumeAttachment's own Meta.ID -- see internal/compute/liveops.go),
	// not server-generated: this is what makes a later vm.remove-device
	// call possible without kyuusha having to separately persist whatever
	// id cloud-hypervisor might have generated for it.
	ID string `json:"id,omitempty"`
}

// PciDeviceInfo is vm.add-disk's response body on success.
type PciDeviceInfo struct {
	ID  string `json:"id"`
	Bdf string `json:"bdf"`
}

// VmRemoveDevice is the request body for PUT vm.remove-device.
type VmRemoveDevice struct {
	ID string `json:"id"`
}

// Resize calls PUT vm.resize.
func (c *Client) Resize(ctx context.Context, req VmResize) error {
	return c.putNoBody(ctx, "vm.resize", req)
}

// AddDisk calls PUT vm.add-disk and returns the PCI device cloud-hypervisor
// assigned it.
func (c *Client) AddDisk(ctx context.Context, cfg DiskConfig) (PciDeviceInfo, error) {
	var out PciDeviceInfo
	err := c.putJSON(ctx, "vm.add-disk", cfg, &out)
	return out, err
}

// RemoveDevice calls PUT vm.remove-device for id (the same id AddDisk was
// given, not cloud-hypervisor's own PciDeviceInfo.ID -- kyuusha's id and
// cloud-hypervisor's device id are the same string by construction, see
// DiskConfig.ID's doc comment).
func (c *Client) RemoveDevice(ctx context.Context, id string) error {
	return c.putNoBody(ctx, "vm.remove-device", VmRemoveDevice{ID: id})
}

func (c *Client) putNoBody(ctx context.Context, path string, body any) error {
	return c.putJSON(ctx, path, body, nil)
}

// putJSON PUTs body (JSON-encoded) to path and, if out is non-nil, decodes
// a 200 response's body into it. A non-2xx status is returned as an error
// carrying the response body verbatim -- cloud-hypervisor's own error
// responses are plain text/JSON describing exactly what went wrong (e.g. a
// vm.resize past the boot-time max= ceiling, or a vm.remove-device for an
// id that no longer exists), and callers (chvmm.Manager, see hotplug.go)
// pattern-match specific known cases against this string rather than a
// structured error code, since cloud-hypervisor's OpenAPI spec doesn't
// define one.
func (c *Client) putJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("chapi: %s: encode request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("chapi: %s: build request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("chapi: %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chapi: %s: status %d: %s", path, resp.StatusCode, string(respBody))
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("chapi: %s: decode response: %w", path, err)
	}
	return nil
}
