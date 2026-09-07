# Dev-only east-west mTLS certs

`ca.crt` + `server.crt`/`server.key` are a self-signed CA and one shared
leaf certificate/key pair generated purely for local/playground use
(`internal/mtls`, `playground/docker-compose.yml`). They protect nothing
real — do not reuse them, do not treat them as secrets, and never point a
real deployment at them.

Unlike `hack/devkeys` (which authenticates *clients* to api-gateway via
JWT), these certs authenticate kyuusha's own services to each other
(east-west: api-gateway→backends, compute→identity/image/network,
block-storage→identity, compute-agent→compute). One shared leaf cert is
used everywhere — as both server and client certificate — since its
Subject Alternative Name list covers every service hostname used in
`playground/docker-compose.yml`; see `internal/mtls`'s package doc for why
a single shared identity is enough for what this currently protects
against (see docs/architecture.md "認可の粒度").

A real deployment should issue a distinct certificate per service (or per
node) from whatever internal CA/PKI it already runs (Vault PKI, cert-manager,
step-ca, ...) instead of using a single shared key — `internal/mtls`'s
`ServerCredentials`/`ClientCredentials` just take file paths, so pointing
`-tls-cert`/`-tls-key`/`-tls-ca` at real per-service files is enough; no
code changes are needed.

Regenerate with:

```
hack/devcerts/generate.sh
```
