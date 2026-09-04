# Multi-stage build for the kyuusha binaries used by docker-compose.yml's
# multi-hypervisor playground. Build a target with e.g.
#   docker build -f docker/Dockerfile --target compute-agent .

FROM golang:1-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/compute ./cmd/compute
RUN CGO_ENABLED=0 go build -o /out/compute-agent ./cmd/compute-agent
RUN CGO_ENABLED=0 go build -o /out/kyuusha ./cmd/kyuusha
RUN CGO_ENABLED=0 go build -o /out/api-gateway ./cmd/api-gateway

FROM alpine:3 AS compute
COPY --from=builder /out/compute /usr/local/bin/compute
ENTRYPOINT ["/usr/local/bin/compute"]

FROM alpine:3 AS api-gateway
COPY --from=builder /out/api-gateway /usr/local/bin/api-gateway
COPY hack/devkeys/jwt-dev.pub /etc/kyuusha/jwt-dev.pub
ENTRYPOINT ["/usr/local/bin/api-gateway", "-jwt-public-key=/etc/kyuusha/jwt-dev.pub"]

FROM alpine:3 AS compute-agent
COPY --from=builder /out/compute-agent /usr/local/bin/compute-agent
ENTRYPOINT ["/usr/local/bin/compute-agent"]

FROM alpine:3 AS kyuusha-cli
COPY --from=builder /out/kyuusha /usr/local/bin/kyuusha
ENTRYPOINT ["/usr/local/bin/kyuusha"]
