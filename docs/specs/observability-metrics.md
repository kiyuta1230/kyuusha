# メトリクス仕様

## 概要

各サービスがOpenTelemetry MeterProvider（Prometheusエクスポータ）経由で、Prometheus
exposition形式の`/metrics`をpull型で公開する。バックエンド非依存で、この形式をscrapeできる
ツールなら何でもよい（playgroundでは実際にPrometheus+Grafanaを使っている——具体的な配線は
[playground/README.md](../../playground/README.md)参照）。gRPC呼び出しのメトリクスは
トレーシングと同じ`otelgrpc`計装から自動的に得られる（[トレーシング仕様](observability-tracing.md)参照）。

## 構成

- `internal/telemetry.SetupMetrics(serviceName)`が各バイナリの起動時に呼ばれ、グローバルな
  `MeterProvider`（Prometheusエクスポータ）を設定し、`/metrics`用の`http.Handler`を返す
- 各バイナリは`-metrics-addr`フラグで指定したアドレス（gRPCポートとは別）でこれを配信する
- トレーシングと異なり無効化フラグはない（pull型なので、誰もscrapeしなければコストはほぼ0）
- Go runtimeメトリクス（goroutine数、GC、メモリ）も`go.opentelemetry.io/contrib/instrumentation/runtime`
  経由で同じMeterProviderに登録される

## エンドポイント一覧

全てのサーバーバイナリ（`cmd/kyuusha`というCLI自体を除く）が`/metrics`を公開する。
2026-09-13のreconcileループ分離（`docs/architecture.md`「コントロールプレーンサービス
自体の可用性」）で増えた`*-reconciler`バイナリも例外ではない。

| サービス | `-metrics-addr`既定値 |
|---|---|
| identity | `:9091` |
| compute | `:9092` |
| api-gateway | `:9093` |
| compute-agent | `:9094` |
| image | `:9095` |
| network | `:9096` |
| block-storage | `:9097` |
| compute-reconciler | `:9098` |
| network-reconciler | `:9099` |
| block-storage-reconciler | `:9100` |

## 主要メトリクス（実測値）

| メトリクス | 種別 | 主なラベル | 用途 |
|---|---|---|---|
| `rpc_server_call_duration_seconds_{count,sum,bucket}` | Histogram | `job`, `rpc_method`, `rpc_response_status_code` | gRPCサーバー側のリクエスト数・レイテンシ・エラー率 |
| `rpc_client_call_duration_seconds_{count,sum,bucket}` | Histogram | 同上 | gRPCクライアント側（呼び出し元から見た計測） |
| `go_goroutine_count` | Gauge | `job`, `instance` | goroutineリーク等の早期検知 |
| `process_resident_memory_bytes` | Gauge | `job`, `instance` | メモリ使用量 |

`rpc_response_status_code`にはgRPCステータスコード（`OK`/`NotFound`/`ResourceExhausted`等、
[認証・認可仕様](authn-authz.md)や[Quota仕様](quota.md)がリクエストを拒否するコードも含む）が
そのまま入るため、エラー率はこのラベルで直接集計できる。

## `/metrics/resources`（compute-agent、VMリソースメトリクス）

上記の`/metrics`（system、このプロセス自身の健全性）とは別に、compute-agentは
`/metrics/resources`として**払い出したVirtualMachine/NetworkInterface/Volumeの
利用状況**を公開する（VM CPU/メモリは2026-09-13実装、残り3メトリクスも同日中に
追加、`docs/architecture.md`「払い出したリソース自身のメトリクス」参照）。実装は
`internal/compute-agent/resourcemetrics.Collector`という独立の`prometheus.Collector`で、
`/metrics`とは別の`prometheus.Registry`から配信する（`resource_version`/Watchに
一切混ぜないという設計上の理由——同セクション参照）。

| メトリクス | 種別 | 主なラベル | 取得元 |
|---|---|---|---|
| `kyuusha_vm_cpu_usage_seconds_total` | Counter | `hypervisor`, `tenant_id`, `vm_id` | cgroup v2 `cpu.stat`の`usage_usec` |
| `kyuusha_vm_memory_usage_bytes` | Gauge | 同上 | cgroup v2 `memory.current` |
| `kyuusha_vm_memory_limit_bytes` | Gauge | 同上 | cgroup v2 `memory.max`（`spec.memory_mb`と一致。cgroupが無制限（`"max"`）を報告する場合は出力されない） |
| `kyuusha_vm_disk_read_bytes_total` / `..._write_bytes_total` | Counter | `hypervisor`, `tenant_id`, `vm_id` | VMMプロセスの`/proc/<pid>/io`（`read_bytes`/`write_bytes`）。root disk+全Volume合算、Volume単位の内訳は無いが、ブロック層を経由しないバックエンド（NFS）でも唯一取れる数字 |
| `kyuusha_networkinterface_receive_bytes_total` / `..._transmit_bytes_total` | Counter | `hypervisor`, `tenant_id`, `interface_id` | tapデバイスの`/sys/class/net/<tap>/statistics/{rx,tx}_bytes`（tap名は`netsetup.TapName(interface_id)`で決定的に導出） |
| `kyuusha_volume_read_bytes_total` / `..._write_bytes_total` / `..._read_ops_total` / `..._write_ops_total` | Counter | `hypervisor`, `tenant_id`, `attachment_id` | バックエンドのブロックデバイスの`/sys/dev/block/<major>:<minor>/stat`。**ブロックデバイス限定**——NFSバックエンドのVolume（identifier=通常ファイル）は対象外（下記参照） |

収集はスクレイプの都度、その場でホスト側の統計ファイルを読むだけ（バックグラウンドの
ポーリングループやキャッシュ状態は持たない）。`internal/compute-agent/cgroup.Apply`が
best-effortである（cgroup v2委譲の無いホストではVMは無制限のまま起動を続ける）のと
同じ理由で、統計が読めなかったVM/NetworkInterface/Volumeはこのエンドポイントに単に
現れない（エラーにもならない）。

**Volumeメトリクスのバックエンド差**: `internal/compute-agent/blockstat.Stats`は
`AttachedVolume.DevicePath`をstatし、ブロックスペシャルファイル（ISCSI/NVME_OF）で
なければ`ok=false`を返して収集側は静かにスキップする——NFS I/OはLinuxのブロック層を
一切経由しないため、`/sys/block/<dev>/stat`相当の場所がそもそも存在しないという
素の事実であり、バグではない。この playground が使っているのは NFS のみなので、
`kyuusha_volume_*`系はここでは常に空、`kyuusha_vm_disk_*`系（VM単位・内訳無し）が
唯一観測できるディスクI/Oの数字になる。

## リソース件数・Quota使用状況（`/metrics`、2026-09-13追加）

`/metrics/resources`のVM CPU/メモリとは別に、「今VMが何台あるか」「Volumeは」
「Subnetは」「Quotaの残りは」といったフリート/業務レベルのメトリクスを`/metrics`
（systemの方）に追加した——こちらはVM payload指標と違い、そもそも`resource_version`/
Watchに触れる話ではなく、既存のgRPCレート等と同じ「制御プレーン自身の状態」の一種
なので、専用エンドポイントに分ける理由がない。

| メトリクス | 種別 | 主なラベル | 提供元バイナリ | 取得元 |
|---|---|---|---|---|
| `kyuusha_tenants_total` | Gauge | `phase` | identity | `internal/identity` TenantのList集計 |
| `kyuusha_images_total` | Gauge | `tenant_id`, `phase` | image | `internal/image` ImageのList集計 |
| `kyuusha_virtualmachines_total` | Gauge | `tenant_id`, `phase` | compute-reconciler | `internal/compute` VirtualMachineのList集計 |
| `kyuusha_volumes_total` | Gauge | `tenant_id`, `phase` | block-storage-reconciler | `internal/block-storage` VolumeのList集計 |
| `kyuusha_volumeattachments_total` | Gauge | `tenant_id`, `phase` | block-storage-reconciler | 同上 VolumeAttachment |
| `kyuusha_subnets_total` | Gauge | `tenant_id`, `phase` | network-reconciler | `internal/network` SubnetのList集計 |
| `kyuusha_networkinterfaces_total` | Gauge | `tenant_id`, `phase` | network-reconciler | 同上 NetworkInterface |
| `kyuusha_network_sg_set_updates_total` | Counter | `kind`（`delta`\|`full`） | network-reconciler | SecurityGroupのアドレス集合の更新をホストへ送った数（[SNAP仕様](snap.md)「アドレス集合の配布」） |
| `kyuusha_compute_agent_sg_set_propagation_seconds` | Histogram | `kind`（`delta`\|`full`） | compute-agent（`/metrics`） | アドレス集合の変化をnetwork-reconcilerが見てから、そのホストで適用し終えるまでの時間（同上。目標は数秒以内） |
| `kyuusha_tenant_quota_used` | Gauge | `tenant_id`, `resource`（`vcpu`\|`memory_mb`\|`vms`はcompute、`volume_gb`はblock-storageが公開） | compute-reconciler / block-storage-reconciler | 各サービスが元々持つ`tenant_usage`のin-memoryアカウンティング（Quota強制ロジックそのもの、`docs/architecture.md`「Quota設計」参照） |
| `kyuusha_tenant_quota_limit` | Gauge | `tenant_id`, `resource`（`vcpu`\|`memory_mb`\|`volume_gb`\|`vms`） | identity | `Tenant.spec.quota`（Quota上限の一次情報源はidentityなので、cross-service RPC無しで直接公開できる） |

**ユーザー数に相当するメトリクスは無い**: kyuushaはUser（ローカルのユーザー
ディレクトリ）というリソースを一切持たない設計——認証は外部OIDC基盤によるJWTで、
`sub`はリクエストの都度JWT claimsとして流れてくるだけの値であり、kyuusha側に
永続化されたUser一覧が存在しない（[認証・認可仕様](authn-authz.md)参照）。
「誰がいつ何をしたか」は`sub`込みで監査ログに残るので、そちら（下記
「誰が・何をしたか」参照）から間接的に把握する形になる。

**リコンサイラー/該当バイナリでのみ登録する理由**: compute/network/block-storageは
API面がN replica安全なステートレスgRPCサーバーなので、そちら側で登録すると
スクレイプのたびにN倍の冗長な`Store.List`全件走査がetcdに飛ぶ。各サービスに
既にある「常に単一インスタンス」なreconcilerバイナリ（コントロールプレーン
サービス自体の可用性」参照）に載せることで、走査は1インスタンス分で済む。
identity/imageにはreconcilerが無い（前者はCRUD+Watchのみ、後者もCreate時の
同期解決かPending/Error止まりで、どちらも非同期reconcileループを持たない設計）
ため、それぞれ唯一のバイナリ自身に直接登録している。

**収集方式**: `/metrics/resources`と同じ「スクレイプの都度、その場でetcdから
List」方式（バックグラウンドのキャッシュ・ポーリングループは持たない）。
`internal/resource.Store.List`はどのみち全件デコードするので（件数だけを安く
数える手段は無い)、この集計コストは各reconcilerが元々行っている定期スイープと
同水準。

`kyuusha_tenant_quota_used`と`kyuusha_tenant_quota_limit`はPromQLで
`tenant_id`/`resource`ラベルを突き合わせれば使用率が出せる:
```
kyuusha_tenant_quota_used / on (tenant_id, resource) kyuusha_tenant_quota_limit
```

## `誰が・何をしたか`の監査ログ

api-gateway経由の全リクエストの監査ログ（`event`/`tenant_id`/`sub`/`rpc_method`等）
はLokiへ集約され、Grafanaの`kyuusha: audit`ダッシュボードから見られる——
詳細なフィールド定義・記録対象範囲は[監査ログ仕様](audit-logging.md)参照。

## playgroundでの可視化

playgroundでは`playground/grafana/provisioning/dashboards/json/`配下の各JSONとして
以下のダッシュボードを自動プロビジョニングしている（構成の詳細は
[playground/README.md](../../playground/README.md)参照）。

- `kyuusha overview`: 全サービス横断のgRPCリクエストレート/エラーレート/p95レイテンシ・goroutine数・常駐メモリ
- `kyuusha: api-gateway` / `identity` / `image` / `compute` / `network` / `block-storage` / `compute-agent`: サービス単体（compute/network/block-storageはAPI+reconcilerペアをまとめて表示）
- `kyuusha: logs`: Lokiバックエンドのログ量・エラー率・ライブログ（`compose_service`フィルタ付き）
- `kyuusha: audit`: 監査ログ専用（上記参照）
- `kyuusha: fleet`: 上記「リソース件数・Quota使用状況」のダッシュボード。Tenant→Image→VM→Volume/VolumeAttachment→Subnet/NetworkInterface→Quotaの順で並べている
- `kyuusha: virtual machines`: VM単体に絞った集約ビュー（件数・tenant別内訳・`kyuusha: compute-agent`と同じVM CPU/メモリのパネルを`$tenant`/`$vm_id`で絞り込み可能な形でまとめ直したもの、2026-09-13にVMディスクI/O・NetworkInterfaceスループット・Volume IOPS/スループット（ブロックデバイスのみ）のパネルを追加）
