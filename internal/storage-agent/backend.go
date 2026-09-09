// Package storageagent implements the real StorageBackend for kyuusha:
// one storage node's ZFS pool (Volumes as zvols) exported over iSCSI (LIO,
// driven via targetcli) -- see docs/architecture.md "block-storageの
// バックエンド抽象化" and docs/specs/volume.md. This is the process that
// actually owns storage, analogous to how compute-agent owns one
// Hypervisor's real VMM processes; internal/block-storage is the only
// client, reached via the StorageBackendService gRPC surface
// (proto/kyuusha/storageagent/v1).
//
// v1 assumes iSCSI, not the NVMe-oF/TCP docs/architecture.md names as its
// stated default: this dev/playground environment's kernel has no
// nvmet-tcp module (nvmet-fc exists but needs Fibre Channel hardware),
// while iSCSI's target_core_mod/iscsi_target_mod load and work here --
// see docs/specs/volume.md for the full explanation. NVMe-oF/TCP remains
// the eventual target for a host that actually supports it.
//
// Every zpool/zfs/targetcli invocation runs via `nsenter --mount=
// /proc/1/ns/mnt` -- found live, the hard way (see docs/specs/volume.md
// "known gotchas"): `zpool create` genuinely fails with a confusing
// ENOENT straight from the ZFS_IOC_POOL_CREATE ioctl when the calling
// process's root is a container's overlayfs mount (confirmed via strace:
// the identical ioctl succeeds immediately on this same host run directly,
// and fails only from inside the container, regardless of privileged
// mode, --pid=host alone, cachefile=none, or bind-mounting the real host
// /dev -- only actually switching into the host's own mount namespace via
// nsenter fixed it). This means the storage-agent container's
// `pid: host` and every path this package touches (the zpool backing
// file) must be valid from the *host's* mount namespace, not just the
// container's -- see playground/docker-compose.yml's identical bind-mount
// source/target trick and cmd/storage-agent/main.go's flag doc.
package storageagent

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Backend drives one storage node's ZFS pool + iSCSI target. One Backend
// per storage-agent process.
type Backend struct {
	// ZpoolName is the already-imported ZFS pool Volumes are created in
	// (as zvols named <ZpoolName>/<volume_id>). Ensuring the pool exists is
	// this package's job too -- see EnsurePool.
	ZpoolName string
	// PortalHost is the "host:port" other containers/hosts should dial to
	// reach this node's iSCSI portal (e.g. "storage-agent:3260" in
	// production, "storage-agent:33260" in the playground -- see
	// docs/specs/volume.md "known gotchas" for why the playground can't use
	// the standard port). ExportVolume creates every target's LIO network
	// portal on 0.0.0.0:<the port named here> explicitly, so this is always
	// the actual listening port, not just an advertised one.
	PortalHost string
}

// portalPort returns PortalHost's port, defaulting to the standard iSCSI
// port 3260 if PortalHost doesn't parse as host:port.
func (b *Backend) portalPort() string {
	if _, port, err := net.SplitHostPort(b.PortalHost); err == nil {
		return port
	}
	return "3260"
}

// iqnPrefix namespaces every target this Backend creates. Kept short and
// stable -- an initiator's local node record is keyed by (portal, this
// IQN), so changing it would orphan any already-attached session's
// bookkeeping.
const iqnPrefix = "iqn.2026-09.io.kyuusha"

func (b *Backend) zvolDataset(volumeID string) string {
	return b.ZpoolName + "/" + volumeID
}

func (b *Backend) zvolPath(volumeID string) string {
	return "/dev/zvol/" + b.ZpoolName + "/" + volumeID
}

func (b *Backend) targetIQN(volumeID string) string {
	return iqnPrefix + ":" + volumeID
}

// EnsurePool makes ZpoolName importable/online, creating it fresh from a
// sparse backing file if it doesn't exist yet -- called once at
// storage-agent startup (cmd/storage-agent/main.go), not per-Volume.
// Idempotent: a pool that already exists (e.g. this process restarted) is
// left alone.
func (b *Backend) EnsurePool(ctx context.Context, backingFile string, sizeGB int64) error {
	if err := run(ctx, "zpool", "list", b.ZpoolName); err == nil {
		return nil // already imported
	}
	if _, err := os.Stat(backingFile); err != nil {
		if err := os.MkdirAll(filepath.Dir(backingFile), 0o755); err != nil {
			return fmt.Errorf("storageagent: create backing file dir: %w", err)
		}
		if err := run(ctx, "truncate", "-s", fmt.Sprintf("%dG", sizeGB), backingFile); err != nil {
			return fmt.Errorf("storageagent: create backing file: %w", err)
		}
	}
	if err := run(ctx, "zpool", "create", b.ZpoolName, backingFile); err != nil {
		return fmt.Errorf("storageagent: zpool create: %w", err)
	}
	return nil
}

// CreateVolume creates volumeID as a new zvol of sizeGB, waiting for its
// device node to actually appear (zfs create returns before udev has
// necessarily finished) before returning.
func (b *Backend) CreateVolume(ctx context.Context, volumeID string, sizeGB int64) error {
	if err := run(ctx, "zfs", "create", "-V", fmt.Sprintf("%dG", sizeGB), b.zvolDataset(volumeID)); err != nil {
		return fmt.Errorf("storageagent: zfs create: %w", err)
	}
	return waitForPath(ctx, b.zvolPath(volumeID))
}

// DeleteVolume destroys volumeID's zvol. The caller (block-storage) is
// responsible for having already called UnexportVolume if it was exported
// -- zfs destroy fails on a zvol still open (e.g. still exported as a LIO
// backstore), and this package doesn't guess at unwinding that itself.
func (b *Backend) DeleteVolume(ctx context.Context, volumeID string) error {
	if err := run(ctx, "zfs", "destroy", b.zvolDataset(volumeID)); err != nil {
		return fmt.Errorf("storageagent: zfs destroy: %w", err)
	}
	return nil
}

// ExportVolume makes volumeID reachable over iSCSI: a dedicated LIO target
// (one target per Volume, named iqn.2026-09.io.kyuusha:<volume_id>) with a
// single LUN backed by the zvol, configured to accept any initiator (no
// per-hypervisor ACL enforcement -- known gap, see docs/specs/volume.md
// "既知の未実装事項": there is no discoverable registry of which
// compute-agent's initiator IQN should be allowed to log into which
// export, matching the same "not the isolation, just the mechanism"
// scoping precedent as internal/compute-agent/cgroup's relationship to
// jailer). Idempotent: re-exporting an already-exported volumeID is a
// no-op past the existence checks, returning the same target IQN/portal.
func (b *Backend) ExportVolume(ctx context.Context, volumeID string) (targetIQN, portal string, err error) {
	iqn := b.targetIQN(volumeID)
	backstorePath := "/backstores/block/" + volumeID
	targetPath := "/iscsi/" + iqn

	if !targetcliExists(ctx, backstorePath) {
		if err := runTargetcli(ctx, "/backstores/block", "create", "name="+volumeID, "dev="+b.zvolPath(volumeID)); err != nil {
			return "", "", fmt.Errorf("storageagent: create backstore: %w", err)
		}
	}

	freshTarget := !targetcliExists(ctx, targetPath)
	if freshTarget {
		// rtslib's "auto_add_default_portal" global preference (on by
		// default) would otherwise give every fresh target a portal on the
		// hardcoded standard port 3260, regardless of what port
		// b.PortalHost actually names -- silently deaf-mute if the two
		// disagree (found live: 3260 is firewalled off in the dev sandbox
		// this was built in, so every export "succeeded" but nothing was
		// ever reachable -- see docs/specs/volume.md "known gotchas").
		// Disabling it and creating the portal explicitly on b.PortalHost's
		// own port keeps what we advertise and what LIO actually listens on
		// in sync no matter which port that is.
		if err := runTargetcli(ctx, "/", "set", "global", "auto_add_default_portal=false"); err != nil {
			return "", "", fmt.Errorf("storageagent: disable auto_add_default_portal: %w", err)
		}
		if err := runTargetcli(ctx, "/iscsi", "create", iqn); err != nil {
			return "", "", fmt.Errorf("storageagent: create iscsi target: %w", err)
		}
		if err := runTargetcli(ctx, targetPath+"/tpg1/portals", "create", "0.0.0.0", b.portalPort()); err != nil {
			return "", "", fmt.Errorf("storageagent: create network portal: %w", err)
		}
		if err := runTargetcli(ctx, targetPath+"/tpg1/luns", "create", backstorePath); err != nil {
			return "", "", fmt.Errorf("storageagent: create lun: %w", err)
		}
		// Any initiator may log in (see doc comment above): no ACL entries,
		// no CHAP.
		if err := runTargetcli(ctx, targetPath+"/tpg1", "set", "attribute", "generate_node_acls=1", "demo_mode_write_protect=0"); err != nil {
			return "", "", fmt.Errorf("storageagent: configure tpg: %w", err)
		}
	}

	return iqn, b.PortalHost, nil
}

// UnexportVolume tears down volumeID's iSCSI target and backstore (but not
// the zvol itself -- see DeleteVolume). A no-op if volumeID was never
// exported, or already unexported.
func (b *Backend) UnexportVolume(ctx context.Context, volumeID string) error {
	iqn := b.targetIQN(volumeID)
	targetPath := "/iscsi/" + iqn
	backstorePath := "/backstores/block/" + volumeID

	if targetcliExists(ctx, targetPath) {
		if err := runTargetcli(ctx, "/iscsi", "delete", iqn); err != nil {
			return fmt.Errorf("storageagent: delete iscsi target: %w", err)
		}
	}
	if targetcliExists(ctx, backstorePath) {
		if err := runTargetcli(ctx, "/backstores/block", "delete", volumeID); err != nil {
			return fmt.Errorf("storageagent: delete backstore: %w", err)
		}
	}
	return nil
}

// targetcliExists reports whether path already exists in the LIO config
// tree, by running `targetcli ls <path>` and checking its exit code --
// targetcli itself has no dedicated "exists" subcommand.
func targetcliExists(ctx context.Context, path string) bool {
	return run(ctx, "targetcli", "ls", path) == nil
}

func runTargetcli(ctx context.Context, path, command string, args ...string) error {
	return run(ctx, "targetcli", append([]string{path, command}, args...)...)
}

// hostNSPrefix is nsenter's own path plus the fixed argument that switches
// into the host's mount namespace (see the package doc comment for why).
// PID 1 is always the host's init as long as this process itself runs with
// pid: host -- true both in the playground container and, trivially and
// harmlessly, if this ever runs directly on a bare host (PID 1 there
// already *is* this process's own init, so nsenter-ing into it is a no-op).
var hostNSPrefix = []string{"nsenter", "--mount=/proc/1/ns/mnt", "--"}

func run(ctx context.Context, name string, args ...string) error {
	full := append(append([]string{}, hostNSPrefix...), append([]string{name}, args...)...)
	out, err := exec.CommandContext(ctx, full[0], full[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitForPath polls for path to exist, up to a few seconds -- covers the
// same "device node creation races the command that triggers it" gap as
// compute-agent's iscsi package waiting for its own by-path symlink.
// Checked via the host mount namespace (see run), not Go's native os.Stat:
// a zvol's /dev/zvol/... symlink is created by udev reacting to a host-
// side kernel event, and never appears in this container's own separate
// /dev (a private tmpfs, confirmed live -- not a view of the host's real
// /dev at all) no matter how long it waits.
func waitForPath(ctx context.Context, path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if run(ctx, "test", "-e", path) == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("storageagent: %s did not appear in time", path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
