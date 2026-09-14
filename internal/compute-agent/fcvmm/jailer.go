package fcvmm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/kiyuta1230/kyuusha/internal/compute-agent/imagestore"
)

// resolveExecPath turns bin (as given to -firecracker-bin/-jailer-bin,
// possibly just a bare name resolved via $PATH, e.g. "firecracker") into an
// absolute path. jailer's --exec-file needs a real filesystem path to copy
// from -- it does not do its own $PATH lookup -- so this has to happen on
// kyuusha's side first.
func resolveExecPath(bin string) (string, error) {
	if filepath.IsAbs(bin) {
		return bin, nil
	}
	return exec.LookPath(bin)
}

// jailChrootDir computes the same path jailer itself will create/reuse --
// <chrootBaseDir>/<basename of the Firecracker binary jailer execs
// into>/<vmID>/root -- so Boot can place resources there *before* jailer
// ever runs (see docs/specs/firecracker-boot.md "jailer": jailer does not
// copy in anything beyond the Firecracker binary itself; every resource
// Firecracker's config references has to already be in place).
func jailChrootDir(chrootBaseDir, fcExecPath, vmID string) string {
	return filepath.Join(chrootBaseDir, filepath.Base(fcExecPath), vmID, "root")
}

// placeReadOnlyResource clones src into dst (inside a jail chroot, via
// imagestore.CloneFile -- copy-on-write where the filesystem supports it,
// a plain copy otherwise) with world-readable permissions -- sufficient for
// the jail's uid/gid to read it without needing an ownership change, which
// matters for src paths that are shared/cached across VMs (this system's
// shared kernel-image cache): chowning a shared file to one VM's jail uid
// would be fine today (every VM's jail shares the same uid/gid, see
// Manager's doc comment) but cloning instead of chowning-in-place avoids
// relying on that not changing.
func placeReadOnlyResource(src, dst string) error {
	if err := imagestore.CloneFile(src, dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o644)
}

// placeWritableResource is placeReadOnlyResource for a resource Firecracker
// (running as jailUID:jailGID after privilege drop) needs to write to --
// Firecracker's root drive, for instance. Ownership is changed to match,
// since world-writable would let anything else that can reach the chroot
// path tamper with it too.
func placeWritableResource(src, dst string, jailUID, jailGID uint32) error {
	if err := placeReadOnlyResource(src, dst); err != nil {
		return err
	}
	return os.Chown(dst, int(jailUID), int(jailGID))
}

// mknodDeviceLike creates a block device node at dst inside the jail with
// the same major:minor as the real device at src (e.g. the /dev/sdX an
// iSCSI login produced, see internal/compute-agent/iscsi) and chowns it to
// the jail's uid/gid so Firecracker can open it read-write once it drops
// privileges. Hard-linking (as done for the kernel/rootfs/seed files)
// doesn't work here: src and the jail's chroot directory tree are on
// different filesystems (a real block device's special file vs. this
// container's own root filesystem), and hard links can't cross that
// boundary.
func mknodDeviceLike(src, dst string, jailUID, jailGID uint32) error {
	var st syscall.Stat_t
	if err := syscall.Stat(src, &st); err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		return fmt.Errorf("%s is not a block device", src)
	}
	dev := unix.Mkdev(unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)))
	if err := syscall.Mknod(dst, syscall.S_IFBLK|0o660, int(dev)); err != nil {
		return fmt.Errorf("mknod %s: %w", dst, err)
	}
	return os.Chown(dst, int(jailUID), int(jailGID))
}

// placeVolumeLike makes src -- an already-resolved Volume path from
// internal/compute-agent/volumeref, either a block device (ISCSI/NVME_OF)
// or a regular file inside an already-mounted NFS export -- visible at dst
// inside the jail, in whichever way keeps the guest's writes landing on
// the real backing store rather than a jail-local copy (persistence is the
// entire point of a Volume, unlike the throwaway rootfs copy
// placeWritableResource makes). mounted reports whether dst is now a bind
// mount the caller must unmount on teardown (mknodDeviceLike's block
// device special file needs no such cleanup: removing it removes only the
// special file, never the real device behind it).
//
// Unlike mknodDeviceLike, the bind-mount path does NOT chown dst: a bind
// mount is the same inode as src, so chowning it would chown the real file
// on the NFS server too. The jail's uid/gid must already be able to
// read/write it -- kyuusha doesn't provision or export this file, so it
// isn't kyuusha's place to change its permissions either (see
// docs/architecture.md「訂正: 責務の境界を...」).
func placeVolumeLike(src, dst string, jailUID, jailGID uint32) (mounted bool, err error) {
	var st syscall.Stat_t
	if err := syscall.Stat(src, &st); err != nil {
		return false, fmt.Errorf("stat %s: %w", src, err)
	}
	if st.Mode&syscall.S_IFMT == syscall.S_IFBLK {
		// A restart (Start after Stop) may find dst already mknod'd from a
		// prior boot of this same VM -- Stop never removes it (only Destroy
		// does; see manager.go's Boot doc comment on jail reuse). Remove and
		// recreate rather than erroring on EEXIST or trusting a stale
		// major:minor blindly (the real device's identity can't change
		// underneath the same Volume, but there's no reason to rely on
		// that when recreating it is just as cheap).
		_ = os.Remove(dst)
		return false, mknodDeviceLike(src, dst, jailUID, jailGID)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return false, fmt.Errorf("create bind-mount target %s: %w", dst, err)
	}
	f.Close()
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return false, fmt.Errorf("bind mount %s onto %s: %w", src, dst, err)
	}
	return true, nil
}
