# CLI仕様

`cmd/kyuusha`（`kyuusha`）。api-gateway経由でのみ通信するクライアント。backendサービス
（compute/identity/image/network/block-storage）を直接叩くことはない。

## 共通

- `-addr`: api-gatewayのアドレス（既定`localhost:8080`）
- `-token`: bearerトークン（省略時は環境変数`$KYUUSHA_TOKEN`）。両方空なら実行時エラー
- `-tenant`: 省略した場合、`-token`（または`$KYUUSHA_TOKEN`）のJWTペイロードを
  署名検証なしでローカルデコードし、`tenant_id`クレームをデフォルト値として使う
  （`resolveTenant`、`cmd/kyuusha/main.go`）。ただし`role`クレームが空でない
  トークン（`admin`/`storage-admin`/`network-admin`/`viewer`）の場合は自動補完
  せず、明示指定を要求する——これらのロールでは`tenant_id`クレームが「操作対象の
  テナント」を意味しないため（[docs/open-questions.md](../open-questions.md)
  「CLIの-tenantフラグをトークンのクレームからデフォルトすべきか」参照）。
  ワイヤプロトコル・サーバー側authzモデルには一切影響しない、CLIだけの利便性機能。
  `kyuusha token mint`自体（これから発行するトークンの`tenant_id`を指定する
  コマンド）はこの自動補完の対象外
- 認証・認可の詳細は[認証・認可仕様](authn-authz.md)を参照

## `kyuusha vm <create|get|list|watch|console|delete|stop|start|resize|migrate|attach-volume|detach-volume|reboot|hard-reboot|add-finalizer|remove-finalizer>`

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-image`(必須、Image ID) `-vcpu`(既定1) `-memory-mb`(既定1024) `-driver-hint`(`firecracker`\|`cloud-hypervisor`、既定は空=サーバー側デフォルト`FIRECRACKER`。Imageの`format`と対応している必要あり——`KERNEL_ROOTFS`はどちらでも可、`QCOW2`は`cloud-hypervisor`必須。[Image仕様](image.md)参照) `-networks`(カンマ区切りNetwork ID。NIC1本ずつ、Subnetはnetworkサービスが選ぶ。先頭が`primary`) `-security-groups`(カンマ区切りSecurityGroup ID。全NICに付ける。省略時は各Networkの既定のグループ) `-subnets`(カンマ区切りSubnet ID。Subnetに固定したいNIC、`-networks`の後に続く) `-zone`(`-networks`を使うなら必須。`-subnets`だけならそのzone。省略時はネットワークなし) `-volumes`(カンマ区切りVolume ID。起動時にattach——attach-before-bootのみ、[Volume仕様](volume.md)参照) `-pci-devices`(カンマ区切り`vendor_id:device_id[:count]`。`cloud-hypervisor`限定、[VirtualMachine仕様](virtual-machine.md)「PCIデバイスパススルー」参照) `-numa-pinned`(スケジューラが選んだ1つの物理NUMAノードへ全vCPU/メモリを固定する。ドライバを問わず使える、[VirtualMachine仕様](virtual-machine.md)「NUMA/CPUピニング」参照) `-user-data-file`(cloud-init user-dataファイルへのパス。省略時は注入しない、[VirtualMachine仕様](virtual-machine.md)「UserData注入」参照) `-wait`(Running/Errorまでブロック) |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` `-finalizer-name`(指定すると`meta.finalizers`にその名前を含むVMだけに絞り込む。[外部システム連携仕様](external-integration.md)「大量Watch対策」参照) |
| `console` | `-tenant`(必須) `-id`(必須) `-tail-bytes`(既定0=サーバ既定値~64KiB、負値で全量) `-follow`(`tail -f`同様に新規出力を流し続ける) |
| `delete` | `-tenant`(必須) `-id`(必須)。`meta.finalizers`が残っていれば実削除されず`deleted_at`がセットされるだけになる（[外部システム連携仕様](external-integration.md)参照）。VMMプロセス停止に加え、jail/runディレクトリ（根本ディスク実体）ごと削除する |
| `stop` | `-tenant`(必須) `-id`(必須) `-force`(既定false。trueなら即SIGKILL、falseならSIGTERM→猶予期間→SIGKILL)。`Running`のみ許可。ディスクは保持される |
| `start` | `-tenant`(必須) `-id`(必須)。`Stopped`のみ許可。`stop`で保持されたディスクを再利用する（[VirtualMachine仕様](virtual-machine.md)/docs/architecture.md「VirtualMachineのライフサイクル状態機械」参照） |
| `resize` | `-tenant`(必須) `-id`(必須) `-vcpu`(必須、新しいvCPU数) `-memory-mb`(必須、新しいメモリ量MB) `-allow-migrate`(既定false。コールドリサイズが現在のHypervisorの容量不足で失敗する場合のみ、コールドマイグレーションを併用して収まる別Hypervisorへ移す——root diskは新HypervisorでImageから作り直される点はプレーンな`migrate`と同じ。ライブリサイズには無視される)。`Stopped`はコールド、`Running`+`CLOUD_HYPERVISOR`はダウンタイム無しのライブリサイズ（[VirtualMachine仕様](virtual-machine.md)「リサイズ」「容量不足時のマイグレーションフォールバック」参照）。`start`/`stop`同様`resource_version`フラグは無い |
| `migrate` | `-tenant`(必須) `-id`(必須) `-target-hypervisor`(省略時は自動選択、現在のHypervisorを除外) `-transfer-root-disk`(既定false。root diskの実際の中身をレジストリ経由で移行先へ転送する——省略時はImageから作り直すだけでゲストの書き込みは失われる。`-migration-registry`がcompute-agent側に設定されていないHypervisorでは失敗する、[VirtualMachine仕様](virtual-machine.md)「ルートディスク転送」参照)。`Stopped`のVMのみ許可 |
| `attach-volume` | `-tenant`(必須) `-id`(必須) `-volume-id`(必須) `-device-hint`(任意)。`Stopped`はコールド、`Running`+`CLOUD_HYPERVISOR`はライブ（[VirtualMachine仕様](virtual-machine.md)「Volume attach/detach」参照） |
| `detach-volume` | `-tenant`(必須) `-id`(必須) `-volume-id`(必須)。`Stopped`/`Running`+`CLOUD_HYPERVISOR`とも可（同上） |
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
| `create` | `-name`(必須、冪等キー) `-display-name` `-max-vcpu` `-max-memory-mb` `-max-volume-gb` `-max-vms` `-max-vcpu-per-vm` `-max-memory-mb-per-vm` `-pci-device-quota`(カンマ区切り`vendor_id:device_id:max_count`。リストに無い組は上限0、[Quota仕様](quota.md)参照) `-max-images` `-max-subnets` `-max-network-interfaces` `-max-ip-reservations` |
| `get` | `-id`(必須) |
| `list` | `-id`(空なら全テナント、admin-only) |
| `watch` | `-id` `-since-resource-version` |
| `update` | `-id`(必須) `-display-name` `-max-vcpu` `-max-memory-mb` `-max-volume-gb` `-max-vms` `-max-vcpu-per-vm` `-max-memory-mb-per-vm` `-pci-device-quota` `-max-images` `-max-subnets` `-max-network-interfaces` `-max-ip-reservations`。明示的に指定したフラグだけがGet→Updateで上書きされ、省略したフィールドは既存値のまま（`-pci-device-quota`を指定した場合はリスト全体を丸ごと置き換える、既存エントリとのマージはしない） |
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

## `kyuusha hostaggregate <create|get|list|update|delete>`（admin-only）

[VMスケジュール仕様](vm-scheduling.md)「HostAggregate」の管理。

| サブコマンド | フラグ |
|---|---|
| `create` | `-name`(必須、冪等キー) `-zone`(必須) `-labels`(カンマ区切り`key=value`) `-hypervisors`(カンマ区切りのHypervisor id) |
| `get` / `delete` | `-id`(必須) |
| `list` | なし |
| `update` | `-id`(必須) `-zone` `-labels` `-hypervisors`。明示的に指定したフラグだけをGet→Updateで丸ごと置き換える |

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

## `kyuusha pool <create|get|list|delete>` / `kyuusha netclass <create|get|list|delete>`

AllocationPool/NetworkClass向け（[network仕様](network.md)「リソース」参照）。仕様が
入れ子の構造なので、`-spec`（protojsonの`AllocationPoolSpec`/`NetworkClassSpec`）または
`-spec-file`で渡す。pool・netclassの作成/削除はテナント横断のロール（`admin`等）だけ。
`netclass get/list`は`-tenant`（既定はトークンのテナント）で、そのテナントが使えるClass
だけを返す。

```sh
kyuusha pool create -name=vlan -spec='{"integer":{"ranges":[{"lo":100,"hi":199}]}}'
kyuusha pool create -name=user-cidr -spec='{"cidr":{"mode":"USER_ANY"}}'
kyuusha netclass create -name=std -spec='{"subnet":{"*":{"refs":[{"poolId":"allocpool-..."},{"poolId":"allocpool-...","name":"vlan_id"}]}},"visibility":"VISIBILITY_PUBLIC"}'
```

## `kyuusha network <create|get|list|delete>`

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant` `-name`(冪等キー) `-class`(必須) `-dns-suffix` `-visibility`(`private`\|`public`) `-shared-with-tenant-ids` `-labels` |
| `get` | `-tenant` `-id`(必須) |
| `list` | `-tenant` または `-all-tenants` |
| `delete` | `-tenant` `-id`(必須) |

## `kyuusha subnet <create|get|list|watch|delete|add-finalizer|remove-finalizer>`

network向け（[network仕様](network.md)参照）。値（VLAN ID等）・CIDR・IPはNetworkClassに
従って実際に払い出される（枯渇時はエラーではなく`Pending`で受理し、出力の
`pending_reason=`に理由が出る）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant` `-name`(冪等キー) `-network`(必須) `-zone`(必須) `-cidr`/`-gateway-ip`(ClassのCIDRプールが利用者指定の場合だけ) `-dns-servers`(カンマ区切り) `-allocatable-ip-ranges`(カンマ区切りの`<開始>-<終了>`範囲) `-labels` `-annotations` |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) または `-all-tenants` |
| `watch` | `-tenant`(必須) または `-all-tenants`、`-since-resource-version` |
| `delete` | `-tenant`(必須) `-id`(必須) |

`update`はgRPC APIとしては存在するがCLIには未実装（Finalizerの付け外しだけ
`add-finalizer`/`remove-finalizer`がある）。

## `kyuusha netif <create|get|list|watch|set-security-groups|delete>`

network向け（[network仕様](network.md)参照）。`NetworkInterfaceService`のCLI名は
`netif`（プロト上のメッセージ名は`NetworkInterface`）。`ip_address`/`mac_address`は
実IPAMにより実際に払い出される。`hypervisor`はVMがRunningの間そのHypervisor
（[network仕様](network.md)参照）。`subnet=`は払い出し元のSubnet（`-network`指定なら
networkサービスが選んだもの）。`security_groups=`は付いているSecurityGroup。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant`(必須) `-name`(冪等キー) `-vm`(VM ID、必須) `-subnet`(Subnet ID) または `-network`＋`-zone`(networkサービスがSubnetを選ぶ) `-security-groups`(カンマ区切り。省略時はNetworkの既定のグループ) `-labels` `-annotations` |
| `get` | `-tenant`(必須) `-id`(必須) |
| `list` | `-tenant`(必須) |
| `watch` | `-tenant`(必須) `-since-resource-version` |
| `set-security-groups` | `-tenant`(必須) `-id`(必須) `-security-groups`(カンマ区切り。丸ごと置き換え、空なら全て外す＝全て拒否) |
| `delete` | `-tenant`(必須) `-id`(必須) |

`update`はgRPC APIとしては存在するがCLIには未実装（同上の理由）。

## `kyuusha ipreservation <create|get|list|delete|add-finalizer|remove-finalizer>`

IPReservation（[network仕様](network.md)「IPReservation」）。`addresses=`は予約できた
アドレス、`pending_reason=`は空きが無くて待っている理由。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant` `-name`(必須、冪等キー) `-network`＋`-zone`(networkサービスがSubnetを選ぶ) または `-subnet` `-address`(特定のアドレス、`-subnet`が必要) `-labels` |
| `get` / `delete` | `-tenant` `-id`(必須) |
| `list` | `-tenant`、または`-all-tenants`(テナント横断のロールのみ) |
| `add-finalizer` / `remove-finalizer` | `-tenant` `-id`(必須) `-finalizer`(必須) |

## `kyuusha secgroup <create|get|list|update|delete>`

SecurityGroup（[network仕様](network.md)「SecurityGroup」）。ルールは
`protocol:port_range:peer`をカンマ区切りで書く——`protocol`は`tcp`/`udp`/`icmp`か空
（全て）、`port_range`は空で全ポート、`peer`はCIDR、`sg=<id>`（`sg=self`はこのグループ）、
`net=<network id>`のどれか（例: `tcp:22:0.0.0.0/0,::sg=self,icmp::net=network-1`）。

| サブコマンド | フラグ |
|---|---|
| `create` | `-tenant` `-name`(必須、冪等キー) `-description` `-ingress` `-egress` `-shared-with-tenant-ids` `-labels` |
| `get` / `delete` | `-tenant` `-id`(必須) |
| `list` | `-tenant`、または`-all-tenants`(テナント横断のロールのみ) |
| `update` | `-tenant` `-id`(必須) `-description` `-ingress` `-egress` `-shared-with-tenant-ids` `-labels`。明示的に指定したフラグだけをGet→Updateで丸ごと置き換える |

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

`volattach create`はblock-storageへ直接発行され、computeを一切経由しない低レベルな
プリミティブ——`-vm`が指すVMの`spec.volumes`には一切触れないため、対象VMが`Stopped`
であってもその次の`Start`では反映されない（`createVolumeAttachments`が
`vm.Spec.Volumes`からしか組み立てないため）。VMへVolumeを実際に届けたい場合は
`kyuusha vm attach-volume`/`detach-volume`（上記「vm」参照）を使うこと。

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
