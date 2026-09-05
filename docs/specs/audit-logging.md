# 監査ログ仕様

## 概要

api-gatewayを通過する全リクエストについて、「誰が・何を・どうなったか」を構造化ログとして記録する。
保存・検索基盤（Loki等）はkyuusha自身のスコープ外で、標準のログ出力として吐くだけに留める。

## 記録される3種類のイベント

| `event` | 発生箇所 | 説明 |
|---|---|---|
| `authn_failed` | `internal/authn`（JWT検証失敗時） | トークンが無効・欠落・期限切れ |
| `authz_denied` | `internal/authz`（OPAがdeny） | 認証は通ったが権限がない |
| `rpc_completed` | `internal/authz`（interceptorがhandlerを呼んだ後） | 認可を通過したRPCの最終結果（成功/業務エラー問わず） |

`authn_failed`の時点では`Claims`がまだ存在しない（検証前に失敗するため）ので、`tenant_id`/`sub`/`role`は
空文字列になる。

## レコードの構造

`internal/audit.Log`が発行する1レコードは以下のフィールドを持つ（JSON、`slog`経由）。

| フィールド | 説明 |
|---|---|
| `audit` | 常に`true`。ログ集約基盤側でのフィルタ用 |
| `event` | 上表のいずれか |
| `rpc_method` | 呼ばれたgRPCフルメソッド名 |
| `request_tenant_id` | リクエスト自体が持つ`tenant_id`（adminが他テナントを操作する場合、`tenant_id`と異なりうる） |
| `tenant_id` / `sub` / `role` | 呼び出し元のJWT claims。`sub`は[認証・認可仕様](authn-authz.md)参照 |
| `trace_id` | リクエストのOTelトレースID（有効な場合のみ）。[トレーシング仕様](observability-tracing.md)のtrace/spanと突き合わせられる |
| `error` | 失敗時のみ。エラーメッセージ |

出力例（実測、playgroundにて）:

```json
{"time":"...","level":"WARN","msg":"audit","audit":true,"event":"authz_denied","rpc_method":"/kyuusha.compute.v1.VirtualMachineService/List","request_tenant_id":"tenant-b495fb3f82bb22ae","tenant_id":"someone-else","sub":"bob@example.com","role":"","trace_id":"d81de36d637a230bd6b7c7a51dd3d0ef","error":"rpc error: code = PermissionDenied desc = not authorized for this tenant"}
```

## streaming RPC（Watch）の扱い

`rpc_completed`はストリーム全体が終了した時点（クライアント切断・ctx cancel等）で1回だけ記録される。
メッセージ単位では記録しない。

## 適用範囲

`internal/authn`/`internal/authz`はapi-gatewayにのみ組み込まれているため、監査ログもapi-gateway経由の
リクエストのみが対象。compute/identityへの直接アクセスや、compute-agentのHypervisor自己登録
（`RegisterHypervisor`、無認証)は対象外（[Hypervisor登録仕様](hypervisor-bootstrap.md)参照）。

## 保存・検索

kyuusha自身は何も永続化しない。標準出力へのJSONログとして吐くだけで、収集・保持・検索は
外部のログ基盤に委ねる（バックエンド非依存）。playgroundでの具体的な配線（Promtail→Loki→Grafana、
`trace_id`から[トレーシング仕様](observability-tracing.md)のJaegerへ直接ジャンプできるderived field等）は
[playground/README.md](../../playground/README.md)を参照。
