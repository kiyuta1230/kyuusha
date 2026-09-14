package main

import (
	"context"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"

	imagev1 "github.com/kiyuta1230/kyuusha/gen/go/kyuusha/image/v1"
)

// imageBuild is the "docker build && docker push" experience
// docs/architecture.md「イメージ作成体験」describes: a Dockerfile's rootfs,
// flattened into the raw ext4 blob KERNEL_ROOTFS Images need (see
// docker/Dockerfile's image-assets stage, which builds
// playground's own test rootfs the exact same way -- truncate + `mkfs.ext4
// -d`), pushed to an OCI registry, and Created as an Image in one command.
//
// This shells out to `docker` and `tar` rather than reimplementing an OCI
// image builder/exporter in Go -- see docs/architecture.md「イメージ作成体験」
// on why kyuusha treats this as a thin CLI wrapper over the existing OCI
// toolchain (not a "難しい分散システムの問題", so a self-built thin wrapper is
// fine; reinventing `docker build` itself would not be). Requires `docker`,
// `tar`, and `mkfs.ext4` (e2fsprogs) on the machine running this command --
// not on any kyuusha service.
//
// QCOW2 is deliberately out of scope: it's a self-contained,
// bootloader-carrying disk format, not a flattened rootfs directory tree,
// so this OCI-layer-flattening pipeline doesn't apply to it (see
// docs/architecture.md「イメージ作成体験」「QCOW2...この変換パスの対象外」).
//
// kyuusha does not yet maintain a catalog of recommended kernels to
// auto-pair with the built rootfs (docs/architecture.md's original vision
// for this), so -kernel-url/-kernel-digest must be supplied explicitly --
// see docs/open-questions.md.
func imageBuild(args []string) {
	fs := flag.NewFlagSet("image build", flag.ExitOnError)
	addr := fs.String("addr", "localhost:8080", "api-gateway address")
	token := fs.String("token", "", "bearer token (default: $KYUUSHA_TOKEN)")
	tenant := fs.String("tenant", "", "tenant ID (required)")
	name := fs.String("name", "", "image name (idempotency key)")
	dockerfile := fs.String("dockerfile", "Dockerfile", "path to the Dockerfile defining the rootfs contents")
	buildContext := fs.String("context", ".", "docker build context directory")
	registry := fs.String("registry", "", "OCI registry host[:port] this command itself connects to in order to push the built rootfs (required)")
	registryRef := fs.String("registry-ref", "", "OCI registry host[:port] to record in the Image's rootfs.url, if different from -registry (default: same as -registry). Needed whenever the registry has a different address from where kyuusha's own services reach it -- e.g. playground's registry is localhost:5000 from this host but registry:5000 from inside compute-agent/image containers (internal Docker DNS); this command pushes via the former but the Image must reference the latter")
	repo := fs.String("repo", "", "OCI repository path within the registry, e.g. myorg/myimage (required)")
	tag := fs.String("tag", "latest", "OCI tag to push the rootfs under")
	plainHTTP := fs.Bool("plain-http", false, "push to -registry over plain HTTP instead of HTTPS (produces an oci+http:// reference -- see internal/compute-agent/imagestore)")
	kernelURL := fs.String("kernel-url", "", "kernel artifact URL or OCI reference to pair with the built rootfs (required)")
	kernelDigest := fs.String("kernel-digest", "", "kernel artifact digest, e.g. sha256:...")
	bootArgs := fs.String("boot-args", "", "direct kernel boot arguments (empty: compute-agent's per-driver default)")
	sizeMB := fs.Int64("size-mb", 0, "ext4 image size in MB (0: auto-sized from the built rootfs contents plus headroom)")
	visibility := fs.String("visibility", "private", "private|public")
	sharedWith := fs.String("shared-with-tenant-ids", "", "comma-separated tenant IDs allowed to see/reference this Image (private only)")
	fs.Parse(args)

	if *tenant == "" || *registry == "" || *repo == "" || *kernelURL == "" {
		fatal("-tenant, -registry, -repo, and -kernel-url are required")
	}

	workDir, err := os.MkdirTemp("", "kyuusha-image-build-")
	if err != nil {
		fatal("create work dir: %v", err)
	}
	defer os.RemoveAll(workDir)

	tmpTag := fmt.Sprintf("kyuusha-image-build-%d", os.Getpid())
	fmt.Fprintf(os.Stderr, "==> docker build -f %s %s\n", *dockerfile, *buildContext)
	runStreamed(exec.Command("docker", "build", "-t", tmpTag, "-f", *dockerfile, *buildContext))
	defer runStreamed(exec.Command("docker", "rmi", "-f", tmpTag))

	containerID := strings.TrimSpace(runCaptured(exec.Command("docker", "create", tmpTag)))
	defer runStreamed(exec.Command("docker", "rm", "-f", containerID))

	rootfsDir := filepath.Join(workDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		fatal("create rootfs dir: %v", err)
	}
	fmt.Fprintln(os.Stderr, "==> docker export | tar -x")
	exportContainer(containerID, rootfsDir)

	size := *sizeMB
	if size == 0 {
		size = autoSizeMB(rootfsDir)
	}
	rootfsImg := filepath.Join(workDir, "rootfs.ext4")
	fmt.Fprintf(os.Stderr, "==> mkfs.ext4 (%dMB)\n", size)
	buildExt4(rootfsDir, rootfsImg, size)

	fmt.Fprintf(os.Stderr, "==> pushing to %s (plain_http=%v) as %s:%s\n", *registry, *plainHTTP, *repo, *tag)
	blob, err := os.ReadFile(rootfsImg)
	if err != nil {
		fatal("read built rootfs: %v", err)
	}
	layerDesc := orasPushFile(context.Background(), *registry, *repo, *tag, *plainHTTP, blob)

	scheme := "oci://"
	if *plainHTTP {
		scheme = "oci+http://"
	}
	ref := *registryRef
	if ref == "" {
		ref = *registry
	}
	rootfsURL := fmt.Sprintf("%s%s/%s:%s", scheme, ref, *repo, *tag)
	fmt.Fprintf(os.Stderr, "==> pushed %s (digest %s)\n", rootfsURL, layerDesc.Digest)

	client := dialImages(*addr)
	ctx := authedContext(context.Background(), *token)
	img, err := client.Create(ctx, &imagev1.CreateImageRequest{
		TenantId: *tenant,
		Name:     *name,
		Spec: &imagev1.ImageSpec{
			Format:              imagev1.ImageFormat_KERNEL_ROOTFS,
			Kernel:              &imagev1.ImageArtifact{Url: *kernelURL, Digest: *kernelDigest},
			Rootfs:              &imagev1.ImageArtifact{Url: rootfsURL, Digest: layerDesc.Digest.String()},
			BootArgs:            *bootArgs,
			Visibility:          parseImageVisibility(*visibility),
			SharedWithTenantIds: splitNonEmpty(*sharedWith),
		},
	})
	if err != nil {
		fatal("create: %v", err)
	}
	printImage(img)
}

// runStreamed runs cmd with its stdout/stderr connected straight to this
// process's own -- the same live progress output `docker build` normally
// shows a human, rather than buffering it into an error message only shown
// on failure.
func runStreamed(cmd *exec.Cmd) {
	cmd.Stdout = os.Stderr // docker build's own progress output is diagnostic, not this command's result -- stderr, like fatal()'s messages
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("%s: %v", strings.Join(cmd.Args, " "), err)
	}
}

// runCaptured is runStreamed for a command whose stdout *is* the result
// (docker create's printed container ID) -- stderr still streams live.
func runCaptured(cmd *exec.Cmd) string {
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fatal("%s: %v", strings.Join(cmd.Args, " "), err)
	}
	return string(out)
}

// exportContainer streams `docker export containerID` straight into `tar
// -x -C destDir` without an intermediate tarball file -- containerID's
// entire filesystem never needs to fit anywhere but destDir itself.
func exportContainer(containerID, destDir string) {
	exportCmd := exec.Command("docker", "export", containerID)
	tarCmd := exec.Command("tar", "-x", "-C", destDir)
	exportCmd.Stderr = os.Stderr
	tarCmd.Stderr = os.Stderr

	pipe, err := exportCmd.StdoutPipe()
	if err != nil {
		fatal("pipe docker export: %v", err)
	}
	tarCmd.Stdin = pipe

	if err := tarCmd.Start(); err != nil {
		fatal("start tar: %v", err)
	}
	if err := exportCmd.Run(); err != nil {
		fatal("docker export: %v", err)
	}
	if err := tarCmd.Wait(); err != nil {
		fatal("tar: %v", err)
	}
}

// autoSizeMBHeadroom is how much larger than the rootfs's own apparent
// byte size the ext4 image is sized -- covers ext4's own metadata/inode
// table overhead plus reserved blocks, with slack for a size_bytes-based
// estimate not perfectly predicting an image built from many small files
// (each consumes at least one block, regardless of apparent size).
const autoSizeMBHeadroom = 1.3

// autoSizeMinMB is the floor for auto-sizing, independent of how small
// rootfsDir's contents are -- mkfs.ext4 itself refuses images below a few
// hundred KB, and a floor this far above that avoids ever brushing against
// that limit for a realistically tiny rootfs.
const autoSizeMinMB = 16

// autoSizeMB sums rootfsDir's apparent file sizes and scales by
// autoSizeMBHeadroom -- the same "truncate to a size that comfortably fits,
// then mkfs.ext4 -d populates it" shape docker/Dockerfile's image-assets
// stage uses with a fixed 64M, generalized to an arbitrary rootfs's actual
// size instead of a value hand-picked for one specific test rootfs.
func autoSizeMB(rootfsDir string) int64 {
	var total int64
	_ = filepath.WalkDir(rootfsDir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	mb := int64(float64(total)/(1024*1024)*autoSizeMBHeadroom) + 1
	if mb < autoSizeMinMB {
		return autoSizeMinMB
	}
	return mb
}

// buildExt4 is docker/Dockerfile's image-assets stage's own
// `truncate -s ... && mkfs.ext4 -d rootfs -F rootfs.ext4` recipe, run here
// instead of at container-build time: create a sparse file of sizeMB, then
// populate an ext4 filesystem into it directly from rootfsDir's contents
// (no mount, no root required -- mkfs.ext4 -d writes the filesystem image
// as a regular file).
func buildExt4(rootfsDir, dest string, sizeMB int64) {
	f, err := os.Create(dest)
	if err != nil {
		fatal("create %s: %v", dest, err)
	}
	if err := f.Truncate(sizeMB * 1024 * 1024); err != nil {
		f.Close()
		fatal("truncate %s: %v", dest, err)
	}
	if err := f.Close(); err != nil {
		fatal("close %s: %v", dest, err)
	}
	runStreamed(exec.Command("mkfs.ext4", "-d", rootfsDir, "-F", dest))
}

// orasPushFile pushes blob to registryAddr/repo:tag as a single-layer OCI
// artifact and returns that layer's descriptor -- the same push shape
// playground/ocitool uses (see its doc comment), duplicated rather than
// shared: that tool is throwaway playground scaffolding this command is the
// real replacement for, not a library the two should both depend on.
func orasPushFile(ctx context.Context, registryAddr, repo, tag string, plainHTTP bool, blob []byte) ocispec.Descriptor {
	repository, err := remote.NewRepository(registryAddr + "/" + repo)
	if err != nil {
		fatal("open repository %s/%s: %v", registryAddr, repo, err)
	}
	repository.PlainHTTP = plainHTTP

	layerDesc, err := oras.PushBytes(ctx, repository, "application/octet-stream", blob)
	if err != nil {
		fatal("push blob: %v", err)
	}
	manifestDesc, err := oras.PackManifest(ctx, repository, oras.PackManifestVersion1_1, "application/vnd.kyuusha.rootfs.v1", oras.PackManifestOptions{
		Layers: []ocispec.Descriptor{layerDesc},
	})
	if err != nil {
		fatal("pack manifest: %v", err)
	}
	if err := repository.Tag(ctx, manifestDesc, tag); err != nil {
		fatal("tag manifest: %v", err)
	}
	return layerDesc
}
