// Package volumeref resolves a Volume's protocol/storage_connection/
// identifier (see docs/specs/volume.md, internal/compute-agent/vmm's
// VolumeAttachInfo) to a local path/device already visible on this host --
// nothing more. kyuusha does not provision, export, log in, or mount
// anything itself (see docs/architecture.md「訂正: 責務の境界を...」): a
// Hypervisor's storage connections (an iSCSI/NVMe-oF session already
// logged in, or an NFS export already mounted) are entirely the
// operator's own doing, a host-level prerequisite exactly like /dev/kvm.
// This package's whole job is finding what that prerequisite already made
// available and handing back a path fcvmm/qemuvmm can wire into a VM's
// jail (copy, or mknod for a block device -- see fcvmm/jailer.go).
//
// There is deliberately no Attach/Detach here (unlike this package's
// predecessor, internal/compute-agent/iscsi, which did real iscsiadm
// login/logout): once discovered, "detaching" a Volume from a VM is pure
// bookkeeping (stop referencing it in that VM's jail) -- there is no
// per-VM network/mount action to undo, since the connection itself
// outlives any single VM and was never established by this code either.
package volumeref

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Connections is the set of storage connections this compute-agent
// declared (via -storage-connections, the same flag value it also sends to
// RegisterHypervisor) that this host actually has established -- name to
// local mount path. Empty path for ISCSI/NVME_OF entries: those discover
// by device identifier under /dev/disk/by-id/, not a path.
type Connections map[string]string

// Resolve finds the local path for one Volume attachment. protocol is one
// of "ISCSI", "NVME_OF", "NFS" (block-storage's StorageProtocol, as a
// string -- see internal/compute-agent/vmm.VolumeAttachInfo); connection
// and identifier come straight from the Volume's own spec.
//
// For ISCSI/NVME_OF, identifier must already be exactly the name of an
// entry under /dev/disk/by-id/ (whatever stable serial/WWN-based name this
// host's udev gave it) -- not reconstructed or guessed at by this package,
// since by-id naming conventions vary by device/driver and only the
// operator who actually provisioned the device knows which one applies.
// For NFS, identifier is a path relative to wherever this host mounted
// that connection.
//
// Polls briefly rather than failing on the first miss: the operator's own
// connection setup and this Volume's registration aren't otherwise
// ordered, so a path that doesn't exist *yet* isn't necessarily wrong.
//
// Also returns the path's real size in bytes -- the one point in the
// whole system where a Volume's actual size is ever observed at all,
// since kyuusha itself never provisions one (see
// docs/open-questions.md「Volumeの申告内容（存在確認・サイズ）が一切検証
// されない」). Callers use it to cross-check against the Volume's
// (otherwise unverifiable) declared size_gb -- see vmm.WarnIfSizeMismatch.
func Resolve(conns Connections, protocol, connection, identifier string) (path string, sizeBytes int64, err error) {
	if connection == "" {
		return "", 0, fmt.Errorf("volumeref: empty storage_connection")
	}
	if identifier == "" {
		return "", 0, fmt.Errorf("volumeref: empty identifier")
	}
	localPath, ok := conns[connection]
	if !ok {
		return "", 0, fmt.Errorf("volumeref: this host has no storage_connection %q declared (have: %v)", connection, connectionNames(conns))
	}

	switch protocol {
	case "ISCSI", "NVME_OF":
		path = filepath.Join("/dev/disk/by-id", identifier)
	case "NFS":
		path = filepath.Join(localPath, identifier)
	default:
		return "", 0, fmt.Errorf("volumeref: unknown protocol %q", protocol)
	}

	if err := waitForPath(path); err != nil {
		return "", 0, err
	}
	sizeBytes, err = realSize(path)
	if err != nil {
		return "", 0, fmt.Errorf("volumeref: determine size of %s: %w", path, err)
	}
	return path, sizeBytes, nil
}

// realSize works identically for a regular file (NFS) and a block device
// special file (ISCSI/NVME_OF, via mknod) -- os.Stat's Size() only reports
// something meaningful for the former, but Seek-to-end works for both,
// since the kernel handles SEEK_END on a block device the same way it
// does on a file.
func realSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Seek(0, io.SeekEnd)
}

func connectionNames(conns Connections) []string {
	names := make([]string, 0, len(conns))
	for name := range conns {
		names = append(names, name)
	}
	return names
}

// waitForPath mirrors internal/storage-agent's identical helper (from
// before this package's predecessor was removed): a device/file that's
// only just become reachable (the operator's connection setup and this
// Volume's registration aren't otherwise ordered) may take a moment to
// appear.
func waitForPath(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("volumeref: %s did not appear in time", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
