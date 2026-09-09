// Package iscsi is compute-agent's iSCSI initiator side: it logs into a
// target storage-agent (internal/storage-agent) already exported (see
// docs/specs/volume.md), and returns the resulting local block device path
// so fcvmm/qemuvmm can pass it to the VMM as an extra drive, the same way
// they already do for the rootfs copy and the cloud-init seed disk. This
// package needs a reachable iscsid (open-iscsi) -- but, unlike everything
// else compute-agent's container starts itself, that has to be the
// *host's* already-running iscsid (its normal systemd unit), not one
// started inside the container -- see compute-agent-entrypoint.sh's own
// comment for why a container-local instance doesn't just fail to help,
// it actively conflicts.
//
// Every iscsiadm invocation runs via `nsenter --net=/proc/1/ns/net` --
// found live, the hard way (see docs/specs/volume.md "known gotchas"): a
// real iSCSI login genuinely fails from inside a container's own network
// namespace. Discovery and the login's initial TCP handshake work fine
// there, but the kernel session iscsid creates via a NETLINK_ISCSI socket
// (confirmed via strace: `sendmsg` on that socket returns ECONNREFUSED)
// does not -- the scsi_transport_iscsi/iscsi_tcp kernel modules only
// register that netlink family in the host's own network namespace, not a
// container's derived one, the same "kernel iSCSI subsystem only really
// works from the host's own namespaces" theme as internal/storage-agent's
// mount-namespace nsenter fix, just for net instead of mount (and, since
// open-iscsi's IPC socket is itself an abstract, network-namespace-scoped
// Unix socket, `nsenter --net` is also what makes iscsiadm here reach the
// *host's* iscsid rather than needing one of its own). This means the
// compute-agent container's `pid: host` and playground/docker-
// compose.yml giving storage-agent a static IP (rather than relying on
// Docker's embedded DNS, which the host's network namespace can't resolve)
// are both required -- see playground/docker-compose.yml's comments. The
// resulting block device is a real host-level device node, so compute-
// agent containers also bind-mount the host's /dev rather than passing
// individual --device entries, so Firecracker/QEMU (running inside the
// container) can actually open it.
package iscsi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// hostNetNSPrefix switches into the host's network namespace -- see the
// package doc comment for why. Mirrors internal/storage-agent's
// hostNSPrefix (mount namespace there, net namespace here); PID 1 is
// always the host's init as long as this container runs with pid: host.
var hostNetNSPrefix = []string{"nsenter", "--net=/proc/1/ns/net", "--"}

// devicePath is the predictable by-path symlink Linux's iSCSI stack
// creates for a session -- stable across reboots/re-logins, unlike
// /dev/sdX, which depends on device enumeration order and can be reused
// once another session detaches. LUN is always 0 -- see
// internal/storage-agent's ExportVolume, which creates exactly one LUN per
// target.
func devicePath(targetIQN, portal string) string {
	return fmt.Sprintf("/dev/disk/by-path/ip-%s-iscsi-%s-lun-0", portal, targetIQN)
}

// Attach logs into targetIQN at portal if not already logged in (checked
// by whether its device path already exists -- sidesteps needing to parse
// iscsiadm's exit codes to distinguish "already logged in" from a real
// failure), and waits for the resulting local block device to appear.
func Attach(ctx context.Context, targetIQN, portal string) (string, error) {
	path := devicePath(targetIQN, portal)
	if _, err := os.Stat(path); err == nil {
		return path, nil // already attached (e.g. a retried Boot)
	}

	if err := runWithRetry(ctx, "iscsiadm", "-m", "discovery", "-t", "sendtargets", "-p", portal); err != nil {
		return "", fmt.Errorf("iscsi: discovery: %w", err)
	}
	if err := runWithRetry(ctx, "iscsiadm", "-m", "node", "-T", targetIQN, "-p", portal, "--login"); err != nil {
		return "", fmt.Errorf("iscsi: login: %w", err)
	}
	if err := waitForPath(ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

// Detach logs out of targetIQN at portal and removes its local node
// record. Best-effort and idempotent: an error here (e.g. the session was
// never established, or already gone) is returned but callers should treat
// it as non-fatal -- same "tap cleanup can't block VM teardown" reasoning
// as internal/compute-agent/netsetup's DeleteTap call sites.
//
// Deliberately takes no context: like DeleteTap, this runs from fcvmm/
// qemuvmm's exit-watch goroutine, well after Boot's own request context
// (the NATS message handler's) is done -- using that context here would
// make exec.CommandContext refuse to even start iscsiadm. Bounded by its
// own internal timeout instead (see run).
func Detach(targetIQN, portal string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, "iscsiadm", "-m", "node", "-T", targetIQN, "-p", portal, "--logout"); err != nil {
		return fmt.Errorf("iscsi: logout: %w", err)
	}
	if err := run(ctx, "iscsiadm", "-m", "node", "-T", targetIQN, "-p", portal, "-o", "delete"); err != nil {
		return fmt.Errorf("iscsi: delete node record: %w", err)
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	full := append(append([]string{}, hostNetNSPrefix...), append([]string{name}, args...)...)
	out, err := exec.CommandContext(ctx, full[0], full[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runWithRetry retries run a few times on failure -- iscsiadm/iscsid can
// still hit an ordinary transient hiccup under host load (the same class
// of flake other timing-sensitive operations in this codebase see), though
// nowhere near as often now that the real bug (see the package doc
// comment) is fixed rather than papered over.
func runWithRetry(ctx context.Context, name string, args ...string) error {
	var lastErr error
	for i := range 5 {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		lastErr = run(ctx, name, args...)
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// waitForPath mirrors internal/storage-agent's identical helper: the
// by-path symlink appears from a separate udev event after login
// succeeds, not synchronously with it. Checked via plain os.Stat, not
// nsenter -- unlike storage-agent's zvol paths, this device node is
// visible in the container's own /dev because that's bind-mounted from
// the host (see the package doc comment).
func waitForPath(ctx context.Context, path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("iscsi: %s did not appear in time", path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
