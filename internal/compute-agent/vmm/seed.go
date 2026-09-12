package vmm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// seedDiskSizeBytes is generous for user-data/meta-data/network-config,
// which per spec.user_data's own doc comment top out around 64KB.
const seedDiskSizeBytes = 1 << 20 // 1MiB

// BuildSeedDisk writes user-data/meta-data (and network-config, if any
// interface has an allocated IP) into their own directory and packs them
// into a cidata-labeled filesystem image via `mkfs.ext4 -d` (the same
// populate-from-directory technique docker/Dockerfile's image-assets stage
// already uses to build the guest rootfs itself) -- not vfat or ISO9660:
// both were tried first and rejected live, because the Firecracker CI
// kernel this playground fetches has neither CONFIG_VFAT_FS nor
// CONFIG_ISO9660_FS built in, only ext4 (see docs/specs/firecracker-boot.md
// "UserData注入" for that whole story). cloud-init's NoCloud datasource
// finds the seed disk by filesystem label via blkid, then mounts it with
// no fixed type, so ext4 works there too, not just for this playground's
// minimal guest. See docs/architecture.md "UserData注入: NoCloud seed
// disk". Returns the path to the generated image.
//
// Shared by every VMM driver (fcvmm, chvmm): building the seed disk
// itself has nothing driver-specific about it, only how each driver
// attaches the resulting image file as a second block device.
//
// kyuusha never inspects or transforms userData: it's opaque cloud-init
// user-data (or a #! script; NoCloud doesn't care) written through
// verbatim, per spec.user_data's existing doc comment.
func BuildSeedDisk(vmDir, vmID, userData string, ifaces []NetIface) (string, error) {
	seedDir := filepath.Join(vmDir, "seed")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		return "", fmt.Errorf("vmm: create seed dir: %w", err)
	}

	// instance-id/local-hostname are the only fields cloud-init's NoCloud
	// datasource requires in meta-data; vmID doubles as both since it's
	// always present (unlike the VM's own, optional, idempotency-key
	// name) and is already a hostname-safe string ("vm-<hex>").
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", vmID, vmID)
	if err := os.WriteFile(filepath.Join(seedDir, "meta-data"), []byte(metaData), 0o644); err != nil {
		return "", fmt.Errorf("vmm: write meta-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "user-data"), []byte(userData), 0o644); err != nil {
		return "", fmt.Errorf("vmm: write user-data: %w", err)
	}
	if netConfig := buildNetworkConfig(ifaces); netConfig != "" {
		if err := os.WriteFile(filepath.Join(seedDir, "network-config"), []byte(netConfig), 0o644); err != nil {
			return "", fmt.Errorf("vmm: write network-config: %w", err)
		}
	}

	// mke2fs needs the target to already be the size the filesystem should
	// be (it reads the existing file/device size when none is given on the
	// command line) -- seedDir's own small directory tree doesn't dictate
	// the image's own size the way it does the files copied into it.
	imgPath := filepath.Join(vmDir, "seed.img")
	f, err := os.Create(imgPath)
	if err != nil {
		return "", fmt.Errorf("vmm: create seed image: %w", err)
	}
	if err := f.Truncate(seedDiskSizeBytes); err != nil {
		f.Close()
		return "", fmt.Errorf("vmm: size seed image: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("vmm: close seed image: %w", err)
	}

	if out, err := exec.Command("mkfs.ext4", "-L", "cidata", "-d", seedDir, "-F", imgPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("vmm: mkfs.ext4: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return imgPath, nil
}

// buildNetworkConfig renders cloud-init's network-config v2 format from
// the same already-resolved interface data netsetup.Wire uses for real tap
// wiring -- one more consumer of the same IP/gateway data, for guests that
// actually run cloud-init (see docs/specs/network.md's kernel-cmdline
// convention for guests that don't). Empty if no interface has an
// allocated IP yet, so a VM with none gets no network-config file at all.
func buildNetworkConfig(ifaces []NetIface) string {
	var eths strings.Builder
	any := false
	for i, ni := range ifaces {
		if ni.IPAddress == "" {
			continue
		}
		any = true
		fmt.Fprintf(&eths, "  eth%d:\n", i)
		fmt.Fprintf(&eths, "    addresses: [%s/%d]\n", ni.IPAddress, ni.PrefixLen)
		if ni.Primary && ni.GatewayIP != "" {
			fmt.Fprintf(&eths, "    gateway4: %s\n", ni.GatewayIP)
		}
	}
	if !any {
		return ""
	}
	return "version: 2\nethernets:\n" + eths.String()
}
