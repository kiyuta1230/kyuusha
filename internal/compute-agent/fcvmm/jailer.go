package fcvmm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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

// placeReadOnlyResource copies src into dst (inside a jail chroot) with
// world-readable permissions -- sufficient for the jail's uid/gid to read
// it without needing an ownership change, which matters for src paths that
// are shared/cached across VMs (this system's shared kernel-image cache):
// chowning a shared file to one VM's jail uid would be fine today (every
// VM's jail shares the same uid/gid, see Manager's doc comment) but copying
// instead of chowning-in-place avoids relying on that not changing.
func placeReadOnlyResource(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
