# 認証・認可仕様

## 概要

client → api-gateway 間（南北）の認証・認可を規定する。api-gatewayが唯一の認証・認可の実施点であり、
backendサービス（compute/identity）自体は認証・認可を持たない。

## 全体フロー

```mermaid
sequenceDiagram
    participant C as Client (kyuusha CLI等)
    participant GW as api-gateway
    participant OPA as OPA (埋め込み)
    participant B as Backend (compute/identity)

    C->>GW: gRPC call + "authorization: Bearer <JWT>"
    GW->>GW: JWT検証 (internal/authn)
    alt 検証失敗
        GW-->>C: Unauthenticated
    end
    GW->>OPA: 認可判定 (internal/authz)
    Note over GW,OPA: input = {claims: {tenant_id, role}, request: {tenant_id}}
    alt 不許可
        GW-->>C: PermissionDenied
    end
    GW->>B: そのままフォワード（backendは無認証）
    B-->>GW: response
    GW-->>C: response
```

## JWT検証（internal/authn）

- アルゴリズム: ECDSA (ES256)。`internal/authn.Verifier`が公開鍵で検証する
- Claims: `tenant_id` (string, 必須。空なら拒否)、`role` (string, 任意)、標準クレーム(`exp`/`iat`等)
- gRPC unary/stream interceptorとして実装。streamはRecvMsgをラップして最初のメッセージ受信時に検証する
- 検証済みClaimsは`context.Context`に格納し、以後のinterceptor（authz）や各RPCハンドラから`authn.FromContext(ctx)`で取得する

### トークン発行

- 現状: `kyuusha token mint`によるローカル署名のみ（`hack/devkeys/jwt-dev.key`）。開発・playground専用
- 本番相当のOIDC発行元（Dex/ORY Hydra等）は未接続

## OPA認可（internal/authz）

- `github.com/open-policy-agent/opa/v1/rego`をGoライブラリとして埋め込み、別サービスは立てない
- ポリシー本体は`internal/authz/policy.rego`（`package kyuusha.authz`）:

```rego
default allow := false
allow if { input.claims.role == "admin" }
allow if {
    input.claims.tenant_id != ""
    input.claims.tenant_id == input.request.tenant_id
}
```

- 評価入力:
  - `input.claims.tenant_id` / `input.claims.role`: JWTのclaim
  - `input.request.tenant_id`: リクエストメッセージの`tenant_id`フィールド（`TenantIDGetter`インターフェースで取得）
- **リクエストメッセージが`tenant_id`フィールドを持たない場合**（例: `CreateTenantRequest`, Hypervisor系の各Request）、`input.request.tenant_id`は空文字列として扱われる。`claims.tenant_id`は空になり得ないため、この場合は事実上 **admin roleのみ許可**になる（ポリシー自体の変更は不要）
- gRPC unary/stream interceptorとして実装。streamはauthnと同様、RecvMsgラップで最初のメッセージ受信時に評価する
- interceptorの適用順序: authn → authz（authzはauthnが設定したClaimsに依存する）

## 適用範囲・既知のギャップ

- api-gatewayで集約検証されるのは南北（client→api-gateway）のみ
- 東西（compute↔identity、compute-agent↔compute）はmTLS設計だが未実装。現状は平文・無認証
- `UpdateVirtualMachineRequest`/`UpdateTenantRequest`は`vm.meta.tenant_id`/`tenant.meta.tenant_id`と別に、認可用の`tenant_id`をトップレベルに持つ。gRPCサーバー側でこの2つの一致を検証し、不一致は`InvalidArgument`で拒否する
