# CLI仕様

`cmd/kyuusha`（`kyuusha`）。api-gateway経由でのみ通信するクライアント。backendサービス
（compute/identity/image/network/block-storage）を直接叩くことはない。

## 共通

- `-addr`: api-gatewayのアドレス（既定`localhost:8080`）
- `-token`: bearerトークン（省略時は環境変数`$KYUUSHA_TOKEN`）。両方空なら実行時エラー
- 認証・認可の詳細は[認証・認可仕様](authn-authz.md)を参照

## `kyuusha vm <create|get|list|watch|console|delete|stop|start|reboot|hard-reboot|add-finalizer|remove-finalizer>`

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-image`(必須、Image ID) `-vcpu`(既定1) `-memory-mb`(既定1024) `-driver-hint`(`firecracker`\|`cloud-hypervisor`、既定は空=サーバー側デフォルト`FIRECRACKER`。Imageの`format`と対応している必要あり——`KERNEL_ROOTFS`はどちらでも可、`QCOW2`は`cloud-hypervisor`必須。[Image仕様](image.md)参照) `-subnets`(カンマ区切りSubnet ID。先頭が`primary`、省略時はネットワークなし) `-user-data-file`(cloud-init user-dataファイルへのパス。省略時は注入しない、[VirtualMachine仕様](virtual-machine.md)「UserData注入」参照) `-wait`(Running/Errorまでブロック) |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` `-finalizer-name`(指定すると`meta.finalizers`にその名前を含むVMだけに絞り込む。[外部システム連携仕様](external-integration.md)「大量Watch対策」参照) |
| `console` | `-tenant`(必須) `-id`(必須) `-tail-bytes`(既定0=サーバ既定値~64KiB、負値で全量) `-follow`(`tail -f`同様に新規出力を流し続ける) |
| `delete` | `-tenant`(必須) `-id`(必須)。`meta.finalizers`が残っていれば実削除されず`deleted_at`がセットされるだけになる（[外部システム連携仕様](external-integration.md)参照）。VMMプロセス停止に加え、jail/runディレクトリ（根本ディスク実体）ごと削除する |
| `stop` | `-tenant`(必須) `-id`(必須) `-force`(既定false。trueなら即SIGKILL、falseならSIGTERM→猶予期間→SIGKILL)。`Running`のみ許可。ディスクは保持される |
| `start` | `-tenant`(必須) `-id`(必須)。`Stopped`のみ許可。`stop`で保持されたディスクを再利用する（[VirtualMachine仕様](virtual-machine.md)/docs/architecture.md「VirtualMachineのライフサイクル状態機械」参照） |
| `reboot` | `-tenant`(必須) `-id`(必須)。サーバー側に専用RPC/状態は無い、CLI側で`stop`→`Stopped`になるまでポーリング→`start`を発行するだけの組み合わせ |
| `hard-reboot` | `reboot`と同じだが`stop`に`force=true`を渡す |
| `add-finalizer` | `-tenant`(必須) `-id`(必須) `-finalizer`(必須、例`acme.corp/network-acl-cleanup`) |
| `remove-finalizer` | `-tenant`(必須) `-id`(必須) `-finalizer`(必須) |

`console`はVMのシリアルコンソール（[Firecracker起動仕様](firecracker-boot.md)/
[cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)参照）を
標準出力へそのまま垂れ流す（フレーミングなし、パイプ/ページャに渡せる）。スケジュール
されたことがない、または実VMMを一度も起動していないVMに対してはエラーになる。

`add-finalizer`/`remove-finalizer`はGet→ローカルで`meta.finalizers`を変更→Updateという
素朴なクライアント側実装（専用RPCは無い）。詳細は[外部システム連携仕様](external-integration.md)
参照。

`update`はgRPC APIとして存在し、上記2つのCLIコマンドが内部で使っている。汎用の
`kyuusha vm update`コマンド自体は無い。

## `kyuusha tenant <create|get|list|watch|update|delete>`

identity向け。`create`は`tenant_id`を持たないリクエストのため**admin-only**
（[認証・認可仕様](authn-authz.md)）。`update`/`delete`はリクエストが`tenant_id`
（対象テナント自身のID）を持つため、対象テナント自身のトークンでも呼べる（自己申告
`tenant_id`＝自分のID、という`identity.Tenant`のself-referentialな構造による。
「Finalizerの所有権」節と同様、adminは常に許可）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-name`(必須、冪等キー) `-display-name` `-max-vcpu` `-max-memory-mb` `-max-volume-gb` `-max-vms` `-max-vcpu-per-vm` `-max-memory-mb-per-vm` |
| `get` | `-id`(必須) |
| `list` | `-id`(空なら全テナント、admin-only) |
| `watch` | `-id` `-since-resource-version` |
| `update` | `-id`(必須) `-display-name` `-max-vcpu` `-max-memory-mb` `-max-volume-gb` `-max-vms` `-max-vcpu-per-vm` `-max-memory-mb-per-vm`。明示的に指定したフラグだけがGet→Updateで上書きされ、省略したフィールドは既存値のまま |
| `delete` | `-id`(必須) |

## `kyuusha hypervisor <get|list|watch|set-schedulable|set-revoked|bootstrap-token>`

compute向け。`get`/`list`/`watch`/`set-schedulable`/`set-revoked`は全て**admin-only**
（`tenant_id`を持たないリクエストのため）。`create`はない（compute-agentの自己登録のみ、
[Hypervisor登録・死活監視仕様](hypervisor-bootstrap.md)）。

| サブコマンド | フラグ |
|---|---|
| `get` | `-id`(必須) |
| `list` | (フラグなし) |
| `watch` | `-since-resource-version` |
| `set-schedulable` | `-id`(必須) `-schedulable`(既定`true`) |
| `set-revoked` | `-id`(必須) `-revoked`(既定`true`)。`true`は`-schedulable=false`も強制する（[Hypervisor登録・死活監視仕様](hypervisor-bootstrap.md)「個体識別と失効」参照） |
| `bootstrap-token create` | `-zone`(必須) `-hypervisor`(省略可、このIDへの登録のみに限定) `-key`(既定`hack/devkeys/jwt-dev.key`) `-ttl`(既定24h) |

`bootstrap-token create`は`token mint`と同じくapi-gateway/`-addr`/`-token`を一切使わない
ローカル署名コマンド——ハイパーバイザーが`RegisterHypervisor`に提示するzone（および任意で
hypervisor_id）スコープ付きJWTを、CLIを実行しているマシン上の秘密鍵ファイルで直接署名して
標準出力に印字するだけ（`internal/bootstraptoken`、
[Hypervisor登録・死活監視仕様](hypervisor-bootstrap.md)参照）。

## `kyuusha image <create|get|list|watch|share|delete>`

image向け。`create`は`-tenant`を持つためadmin-onlyではない（テナント自身が自分のImageを作れる）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-format`(`kernel_rootfs`\|`qcow2`、必須) `-kernel-url` `-kernel-digest` `-rootfs-url` `-rootfs-digest` `-disk-url` `-disk-digest` `-boot-args` `-visibility`(`private`\|`public`、既定`private`) `-shared-with-tenant-ids`(カンマ区切り、`private`時のみ意味を持つ) |
| `get` | `-tenant`(必須) `-id`(必須)。所有テナントでなくてもPUBLIC/共有されたImageなら見える |
| `list` | `-tenant`(必須)。自分のImage + 見えるPUBLIC/共有Image |
| `watch` | `-tenant`(必須) `-since-resource-version`。同上 |
| `share` | `-tenant`(必須、所有テナントである必要あり) `-id`(必須) `-visibility`(既定`private`) `-shared-with-tenant-ids`(既存の一覧を丸ごと置き換える) |
| `delete` | `-tenant`(必須、所有テナントである必要あり) `-id`(必須) |

`share`が呼ぶ`SetVisibility`は[Image仕様](image.md)「マルチテナント対応（可視性/共有）」
参照——kernel/rootfs/disk自体を変える汎用`update`は存在しない（意図的に無い）。

## `kyuusha subnet <create|get|list|watch|delete>`

network向け（[network仕様](network.md)参照）。`create`は`-tenant`を持つためadmin-onlyでは
ない。`vlan_id`/`ip_address`はIPAMにより実際に払い出される（プール枯渇時はエラーではなく
`Pending`で受理、[network仕様](network.md)参照）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-zone`(必須) `-cidr`(必須、例`10.0.1.0/24`) `-gateway-ip` `-dns-servers`(カンマ区切り) `-dns-suffix` `-mesh-group`(同じ値を持つSubnet同士の既定許可を宣言。ACL強制はまだ) `-allocatable-ip-ranges`(カンマ区切りの`<開始>-<終了>`範囲。未指定ならCIDR全体) |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` |
| `delete` | `-tenant`(必須) `-id`(必須) |

`update`はgRPC APIとしては存在するがCLIには未実装（CIDR/zone等をCreate後に変える
実運用上のユースケースが今のところ無いため）。

## `kyuusha netif <create|get|list|watch|delete>`

network向け（[network仕様](network.md)参照）。`NetworkInterfaceService`のCLI名は
`netif`（プロト上のメッセージ名は`NetworkInterface`）。`ip_address`/`mac_address`は
実IPAMにより実際に払い出される。`hypervisor`は実バックエンド/tap配線報告連携がまだ
ないため常に空（[network仕様](network.md)参照）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-vm`(VM ID、必須) `-subnet`(Subnet ID、必須) |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` |
| `delete` | `-tenant`(必須) `-id`(必須) |

`update`はgRPC APIとしては存在するがCLIには未実装（同上の理由）。

## `kyuusha volume <create|get|list|watch|delete>`

block-storage向け（[Volume仕様](volume.md)参照）。`create`は`-tenant`を持つため
admin-onlyではない。kyuushaはボリュームを作成/削除しない——既存の（iSCSI/NVMe-oF/NFS
で到達可能な）ボリュームへの参照を登録するだけ。`size_gb`/`protocol`/
`storage_connection`/`identifier`のバリデーションと、参照する`StorageConnection`が
実在することが通れば`Pending`で受理され、非同期の検証（[Volume仕様](volume.md)
「検証フロー」）が終わり次第`Ready`になる。検証で実測サイズが`-size-gb`と10%以上
ずれていた場合、`spec.size_gb`自体が実測値へ補正される（`get`/`list`/`watch`の
出力にそのまま反映される）——ストレージ管理者の入力ミスを、システムが検知した
時点で直すという判断（[Volume仕様](volume.md)「検証フロー」参照）。`conditions`には
`IdentifierVerified`（実在確認）、`SizeMatchesDeclaration`（補正後は実質常にTrue）、
`QuotaExceededAfterCorrection`（補正の結果Quotaを超えた場合のみTrue、この場合も
`Ready`への昇格自体はブロックしない）が入る。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-size-gb`(必須、自己申告値でQuota計算にのみ使う) `-protocol`(`ISCSI`/`NVME_OF`/`NFS`、必須) `-storage-connection`(必須) `-identifier`(必須) `-annotations`(省略可、`k=v,k=v`形式、kyuusha自身は解釈しない参考情報) |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` |
| `delete` | `-tenant`(必須) `-id`(必須) |

`update`（リサイズ等）自体が存在しない（[Volume仕様](volume.md)参照）。

## `kyuusha storageconn <create|get|list|watch|delete>`（admin-only）

block-storage向け（[Volume仕様](volume.md)「StorageConnection」参照）。テナント非
スコープ（Hypervisorと同じ）なので、どのサブコマンドも`-tenant`を持たず、admin-only。

| サブコマンド | フラグ |
|---|---|
| `create` | `-name`(冪等キー、必須) `-zones`(カンマ区切りAZ一覧、必須) `-annotations`(省略可、`k=v,k=v`形式) |
| `get` | `-id`(必須) |
| `list` | （フラグ無し） |
| `watch` | `-since-resource-version` |
| `delete` | `-id`(必須) |

## `kyuusha volattach <create|get|list|watch|delete>`

block-storage向け（[Volume仕様](volume.md)参照）。`VolumeAttachmentService`のCLI名は
`volattach`。同一`volume_id`について非`Deleting`なVolumeAttachmentは同時に1つまで
（排他制御。他の有効なアタッチメントが残っている間のCreateは拒否ではなく`Pending`で
受理され、10秒毎に再試行される）。`get`/`list`/`watch`の出力の`device_path`/`hypervisor`は、
そのVolumeを使う実VMが一度でも起動していれば、compute-agentが見つけた実パスと起動先
Hypervisorが入る（[Volume仕様](volume.md)「device_path/hypervisorの報告」参照）。
まだどのVMも起動していなければ空。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-vm`(VM ID、必須) `-volume`(Volume ID、必須) `-device-hint`(省略可) |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` |
| `delete` | `-tenant`(必須) `-id`(必須) |

## `kyuusha token mint`

devonly。ローカルのECDSA秘密鍵でJWTを署名するだけで、実際のOIDC発行元を経由しない
（[認証・認可仕様](authn-authz.md)参照）。

| フラグ | 説明 |
|---|---|
| `-key` | 署名鍵ファイル（既定`hack/devkeys/jwt-dev.key`） |
| `-tenant` | `tenant_id`クレーム（必須） |
| `-role` | `role`クレーム: `""`(既定)/`admin`(全テナント横断)/`storage-admin`(全テナントのblock-storage RPCのみ)。任意、[認証・認可仕様](authn-authz.md)参照 |
| `-tenant-role` | `tenant_role`クレーム: `""`(既定、a.k.a. member、自テナント内read/write)/`viewer`(自テナント内read-only)。任意、[認証・認可仕様](authn-authz.md)参照 |
| `-sub` | `sub`クレーム（誰が。任意、[監査ログ仕様](audit-logging.md)参照。自己申告で認可判定には使われない） |
| `-ttl` | トークン有効期限（既定1時間） |
