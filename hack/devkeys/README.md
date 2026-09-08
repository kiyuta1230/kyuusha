# Dev-only JWT keypair

`jwt-dev.key`/`jwt-dev.pub` is an ECDSA P-256 keypair generated purely for
local/playground use (`cmd/api-gateway -jwt-public-key`, `cmd/kyuusha token
mint -key`, `cmd/compute -bootstrap-token-public-key`, `kyuusha hypervisor
bootstrap-token create -key`). It protects nothing real — do not reuse it,
do not treat it as a secret, and never point a real deployment at it.

Real deployments verify client JWTs against an actual OIDC provider's JWKS
(`cmd/api-gateway -jwt-jwks-url`; see docs/specs/authn-authz.md) instead of
a static key file, and would mint Hypervisor bootstrap tokens
(`internal/bootstraptoken`) with their own separate signing key rather than
reusing this one — sharing a key here is a dev-only simplification.

Regenerate with:

```
openssl ecparam -name prime256v1 -genkey -noout -out jwt-dev.key
openssl ec -in jwt-dev.key -pubout -out jwt-dev.pub
```

`bootstrap-token-zone-a.jwt` is a 10-year `-zone=zone-a` bootstrap token
signed with this same dev key (see docs/specs/hypervisor-bootstrap.md), baked
into `docker/Dockerfile`'s compute-agent stage for the playground (all 3
compute-agents there register into zone-a). Regenerate it after regenerating
`jwt-dev.key` with:

```
go run ./cmd/kyuusha hypervisor bootstrap-token create -zone=zone-a -ttl=87600h > bootstrap-token-zone-a.jwt
```
