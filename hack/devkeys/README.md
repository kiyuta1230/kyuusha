# Dev-only JWT keypair

`jwt-dev.key`/`jwt-dev.pub` is an ECDSA P-256 keypair generated purely for
local/playground use (`cmd/compute -jwt-public-key`, `cmd/kyuusha token mint
-key`). It protects nothing real — do not reuse it, do not treat it as a
secret, and never point a real deployment at it.

Real deployments verify against an actual OIDC provider's JWKS (Dex/Hydra;
see docs/architecture.md "認証・認可とHypervisor登録") instead of a static
key file.

Regenerate with:

```
openssl ecparam -name prime256v1 -genkey -noout -out jwt-dev.key
openssl ec -in jwt-dev.key -pubout -out jwt-dev.pub
```
