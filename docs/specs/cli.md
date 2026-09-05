# CLI仕様

`cmd/kyuusha`（`kyuusha`）。api-gateway経由でのみ通信するクライアント。backendサービス
（compute/identity/image）を直接叩くことはない。

## 共通

- `-addr`: api-gatewayのアドレス（既定`localhost:8080`）
- `-token`: bearerトークン（省略時は環境変数`$KYUUSHA_TOKEN`）。両方空なら実行時エラー
- 認証・認可の詳細は[認証・認可仕様](authn-authz.md)を参照

## `kyuusha vm <create|get|list|watch>`

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-image`(必須、Image ID) `-vcpu`(既定1) `-memory-mb`(既定1024) `-recovery-policy`(`none`\|`self-heal`、既定`none`) `-wait`(Running/Errorまでブロック) |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` |

`update`/`delete`はgRPC APIとしては存在するがCLIには未実装。

## `kyuusha tenant <create|get|list|watch>`

identity向け。`create`は`tenant_id`を持たないリクエストのため**admin-only**
（[認証・認可仕様](authn-authz.md)）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-name`(必須、冪等キー) `-display-name` `-max-vcpu` `-max-memory-mb` `-max-volume-gb` `-max-vms` `-max-vcpu-per-vm` `-max-memory-mb-per-vm` |
| `get` | `-id`(必須) |
| `list` | `-id`(空なら全テナント、admin-only) |
| `watch` | `-id` `-since-resource-version` |

## `kyuusha hypervisor <get|list|watch|set-schedulable>`

compute向け。全サブコマンドが**admin-only**（`tenant_id`を持たないリクエストのため）。
`create`はない（compute-agentの自己登録のみ、[Hypervisor登録・死活監視仕様](hypervisor-bootstrap.md)）。

| サブコマンド | フラグ |
|---|---|
| `get` | `-id`(必須) |
| `list` | (フラグなし) |
| `watch` | `-since-resource-version` |
| `set-schedulable` | `-id`(必須) `-schedulable`(既定`true`) |

## `kyuusha image <create|get|list|watch>`

image向け。`create`は`-tenant`を持つためadmin-onlyではない（テナント自身が自分のImageを作れる）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-format`(`kernel_rootfs`\|`qcow2`、必須) `-kernel-url` `-kernel-digest` `-rootfs-url` `-rootfs-digest` `-disk-url` `-disk-digest` `-boot-args` |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` |

`delete`はgRPC APIとしては存在するがCLIには未実装。

## `kyuusha token mint`

devonly。ローカルのECDSA秘密鍵でJWTを署名するだけで、実際のOIDC発行元を経由しない
（[認証・認可仕様](authn-authz.md)参照）。

| フラグ | 説明 |
|---|---|
| `-key` | 署名鍵ファイル（既定`hack/devkeys/jwt-dev.key`） |
| `-tenant` | `tenant_id`クレーム（必須） |
| `-role` | `role`クレーム（例: `admin`。任意） |
| `-sub` | `sub`クレーム（誰が。任意、[監査ログ仕様](audit-logging.md)参照。自己申告で認可判定には使われない） |
| `-ttl` | トークン有効期限（既定1時間） |
