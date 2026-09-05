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

FROM alpine:3 AS compute
COPY --from=builder /out/compute /usr/local/bin/compute
ENTRYPOINT ["/usr/local/bin/compute"]

FROM alpine:3 AS api-gateway
COPY --from=builder /out/api-gateway /usr/local/bin/api-gateway
COPY hack/devkeys/jwt-dev.pub /etc/kyuusha/jwt-dev.pub
ENTRYPOINT ["/usr/local/bin/api-gateway", "-jwt-public-key=/etc/kyuusha/jwt-dev.pub"]

FROM alpine:3 AS compute-agent
# Firecracker release binary (statically linked, no glibc dependency) --
# real driver_hint=FIRECRACKER VM boot, see internal/compute-agent/fcvmm.
RUN apk add --no-cache --virtual .fc-fetch curl \
    && ARCH="$(uname -m)" && FC_VERSION=v1.16.1 \
    && curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz" \
       | tar -xz \
    && mv "release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker \
    && chmod +x /usr/local/bin/firecracker \
    && rm -rf "release-${FC_VERSION}-${ARCH}" \
    && apk del .fc-fetch
COPY --from=builder /out/compute-agent /usr/local/bin/compute-agent
ENTRYPOINT ["/usr/local/bin/compute-agent"]

FROM alpine:3 AS kyuusha-cli
COPY --from=builder /out/kyuusha /usr/local/bin/kyuusha
ENTRYPOINT ["/usr/local/bin/kyuusha"]

FROM alpine:3 AS identity
COPY --from=builder /out/identity /usr/local/bin/identity
ENTRYPOINT ["/usr/local/bin/identity"]

FROM alpine:3 AS image
COPY --from=builder /out/image /usr/local/bin/image
ENTRYPOINT ["/usr/local/bin/image"]

FROM alpine:3 AS network
COPY --from=builder /out/network /usr/local/bin/network
ENTRYPOINT ["/usr/local/bin/network"]

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
