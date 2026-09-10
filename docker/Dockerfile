# Multi-stage build for the kyuusha binaries used by
# playground/docker-compose.yml's multi-hypervisor playground. Build a
# target with e.g.
#   docker build -f docker/Dockerfile --target compute-agent .
#
# Also builds `image-assets-server` -- a static file server for the
# playground's Firecracker kernel/rootfs (see docs/specs/firecracker-boot.md
# and fc-guest-init.sh below). These are prepared at *build* time (not
# fetched by a running container) since this sandbox's container runtime
# network doesn't reach the public internet the way `docker build` does.

FROM golang:1-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/compute ./cmd/compute
RUN CGO_ENABLED=0 go build -o /out/compute-agent ./cmd/compute-agent
RUN CGO_ENABLED=0 go build -o /out/kyuusha ./cmd/kyuusha
RUN CGO_ENABLED=0 go build -o /out/api-gateway ./cmd/api-gateway
RUN CGO_ENABLED=0 go build -o /out/identity ./cmd/identity
RUN CGO_ENABLED=0 go build -o /out/image ./cmd/image
RUN CGO_ENABLED=0 go build -o /out/network ./cmd/network
RUN CGO_ENABLED=0 go build -o /out/block-storage ./cmd/block-storage
RUN CGO_ENABLED=0 go build -o /out/storage-agent ./cmd/storage-agent

FROM alpine:3 AS compute
COPY --from=builder /out/compute /usr/local/bin/compute
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
COPY hack/devkeys/jwt-dev.pub /etc/kyuusha/jwt-dev.pub
ENTRYPOINT ["/usr/local/bin/compute", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt", "-bootstrap-token-public-key=/etc/kyuusha/jwt-dev.pub"]

FROM alpine:3 AS api-gateway
COPY --from=builder /out/api-gateway /usr/local/bin/api-gateway
COPY hack/devkeys/jwt-dev.pub /etc/kyuusha/jwt-dev.pub
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
ENTRYPOINT ["/usr/local/bin/api-gateway", "-jwt-public-key=/etc/kyuusha/jwt-dev.pub", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt"]

# Ubuntu-based, not Alpine like every other service here (except
# storage-agent, also Ubuntu for its own unrelated packaging reason above):
# found live that Alpine's musl-linked open-iscsi package (iscsid) reliably
# crashes under load in this environment ("iscsid: sendmsg: bug? ctrl_fd 4",
# confirmed via /proc/<pid>/status showing it a zombie right after, and via
# a dedicated restart-loop + long retry budget in
# compute-agent-entrypoint.sh/internal/compute-agent/iscsi that still
# couldn't outrun it -- every single retry re-triggered another crash,
# never stabilizing). Ubuntu/Debian's glibc-linked open-iscsi build doesn't
# hit this at all in the same testing -- see docs/specs/volume.md "known
# gotchas".
FROM ubuntu:24.04 AS compute-agent
# Firecracker release binary (statically linked, no glibc dependency) --
# real driver_hint=FIRECRACKER VM boot, see internal/compute-agent/fcvmm.
# jailer ships in the exact same release tarball -- every driver_hint=
# FIRECRACKER VM is exec'd through it (chroot + uid/gid privilege drop;
# Firecracker's own built-in seccomp filter applies regardless of jailer,
# on by default unless -no-seccomp -- see docs/specs/firecracker-boot.md
# "jailer"), unlike driver_hint=QEMU, for which an equivalent is still an
# open question (docs/open-questions.md).
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && ARCH="$(uname -m)" && FC_VERSION=v1.16.1 \
    && curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz" \
       | tar -xz \
    && mv "release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker \
    && mv "release-${FC_VERSION}-${ARCH}/jailer-${FC_VERSION}-${ARCH}" /usr/local/bin/jailer \
    && chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer \
    && rm -rf "release-${FC_VERSION}-${ARCH}" \
    && apt-get purge -y curl && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
# qemu-system-x86_64 -- real driver_hint=QEMU VM boot, see
# internal/compute-agent/qemuvmm and docs/specs/qemu-boot.md.
#
# e2fsprogs: builds the cloud-init NoCloud seed disk as an ext4 image via
# `mkfs.ext4 -d` (see internal/compute-agent/vmm/seed.go and
# docs/architecture.md "UserData注入: NoCloud seed disk").
#
# open-iscsi: the iscsiadm CLI internal/compute-agent/iscsi shells out to,
# for attaching Volumes exported by storage-agent -- see docs/specs/
# volume.md. Just the CLI: iscsid itself has to be the *host's* own
# already-running instance, not one started in this container -- see
# internal/compute-agent/iscsi's package doc comment and
# compute-agent-entrypoint.sh for why. Installing the open-iscsi package
# here is still needed for iscsiadm/its libraries, even though this
# image's own iscsid is never actually run.
#
# iproute2: `ip` (internal/compute-agent/netsetup's tap/bridge wiring) --
# unlike Alpine, Ubuntu's minimal base doesn't carry this by default.
RUN apt-get update && apt-get install -y --no-install-recommends \
      qemu-system-x86 e2fsprogs open-iscsi iproute2 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/compute-agent /usr/local/bin/compute-agent
COPY docker/compute-agent-entrypoint.sh /usr/local/bin/compute-agent-entrypoint.sh
RUN chmod +x /usr/local/bin/compute-agent-entrypoint.sh
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
# All 3 playground compute-agents register into zone-a (see
# playground/docker-compose.yml), so one baked-in dev token covers all of
# them; a hypervisor in a different zone needs its own token (`kyuusha
# hypervisor bootstrap-token create -zone=...`) mounted/COPYed in instead.
COPY hack/devkeys/bootstrap-token-zone-a.jwt /etc/kyuusha/bootstrap-token.jwt
ENTRYPOINT ["/usr/local/bin/compute-agent-entrypoint.sh", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt", "-bootstrap-token-file=/etc/kyuusha/bootstrap-token.jwt"]

FROM alpine:3 AS kyuusha-cli
COPY --from=builder /out/kyuusha /usr/local/bin/kyuusha
ENTRYPOINT ["/usr/local/bin/kyuusha"]

FROM alpine:3 AS identity
COPY --from=builder /out/identity /usr/local/bin/identity
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
ENTRYPOINT ["/usr/local/bin/identity", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt"]

FROM alpine:3 AS image
COPY --from=builder /out/image /usr/local/bin/image
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
ENTRYPOINT ["/usr/local/bin/image", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt"]

FROM alpine:3 AS network
COPY --from=builder /out/network /usr/local/bin/network
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
ENTRYPOINT ["/usr/local/bin/network", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt"]

FROM alpine:3 AS block-storage
COPY --from=builder /out/block-storage /usr/local/bin/block-storage
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
ENTRYPOINT ["/usr/local/bin/block-storage", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt"]

# storage-agent needs zfsutils-linux + targetcli-fb (internal/storage-agent
# shells out to `zfs`/`zpool`/`targetcli`) -- Ubuntu-based, not Alpine or
# Debian like every other service here: Alpine doesn't reliably package
# targetcli-fb, and Debian's default `main` component doesn't carry
# zfsutils-linux at all (it's gated behind `contrib`, for CDDL-licensing
# reasons Ubuntu doesn't apply the same way -- confirmed live: the package
# install that failed here on debian:bookworm-slim is the exact one that
# succeeded cleanly on this repo's own (Ubuntu) dev host). `universe` has
# to be added explicitly first -- Ubuntu's minimal container base only
# enables `main`/`restricted` by default, unlike a full desktop/server
# install.
FROM ubuntu:24.04 AS storage-agent
RUN sed -i 's/^Components: main restricted$/Components: main restricted universe/' /etc/apt/sources.list.d/ubuntu.sources \
    && apt-get update && apt-get install -y --no-install-recommends \
      zfsutils-linux targetcli-fb \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/storage-agent /usr/local/bin/storage-agent
COPY hack/devcerts/ca.crt hack/devcerts/server.crt hack/devcerts/server.key /etc/kyuusha/tls/
ENTRYPOINT ["/usr/local/bin/storage-agent", "-tls-cert=/etc/kyuusha/tls/server.crt", "-tls-key=/etc/kyuusha/tls/server.key", "-tls-ca=/etc/kyuusha/tls/ca.crt"]

# ---- image-assets: prebuilt kernel + minimal rootfs for the playground's ----
# Firecracker milestone (see docs/specs/firecracker-boot.md). A real Firecracker
# CI-published vmlinux (built with the virtio drivers a Firecracker guest
# needs) paired with a from-scratch Alpine rootfs -- not the getting-started
# guide's Ubuntu squashfs, since Alpine boots comfortably in far less RAM.
FROM debian:bookworm-slim AS image-assets
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates e2fsprogs \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /assets
RUN ARCH="$(uname -m)" \
    && S3="https://s3.amazonaws.com/spec.ccfc.min" \
    && CI_ARTIFACTS_PREFIX=$(curl -fsSL "$S3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
         | grep -oP "(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)" | sort | tail -1) \
    && latest_kernel_key=$(curl -fsSL "$S3?list-type=2&prefix=${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-" \
         | grep -oP "(?<=<Key>)(${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" | sort -V | tail -1) \
    && curl -fsSL -o vmlinux "$S3/${latest_kernel_key}"
RUN curl -fsSL -o alpine-minirootfs.tar.gz \
      https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.10-x86_64.tar.gz \
    && mkdir -p rootfs \
    && tar -xzf alpine-minirootfs.tar.gz -C rootfs
COPY docker/fc-guest-init.sh rootfs/init
RUN chmod +x rootfs/init \
    && truncate -s 64M rootfs.ext4 \
    && mkfs.ext4 -d rootfs -F rootfs.ext4

FROM nginx:1-alpine AS image-assets-server
COPY --from=image-assets /assets/vmlinux /usr/share/nginx/html/vmlinux
COPY --from=image-assets /assets/rootfs.ext4 /usr/share/nginx/html/rootfs.ext4
