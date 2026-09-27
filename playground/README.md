# Playground

`docker-compose.yml`（このディレクトリ）が起動する、ローカルの多ハイパーバイザー環境。
リポジトリルートから実行する。

## 起動

```sh
docker compose -f playground/docker-compose.yml up -d --build
```

`compute-agent-1/2/3`は`/dev/kvm`を要求する（実Firecracker起動、
[Firecracker起動仕様](../docs/specs/firecracker-boot.md)参照）。ホストにKVMがない場合、
VM作成自体はできるがゲストの実起動は失敗する（スタックの他の部分には影響しない）。

## 動作確認

```sh
./playground/scenario.sh
```

Tenant作成→Image作成→Ready待ち→VM作成（実Firecracker起動、`/dev/kvm`があれば）→スケジュール→
Quota強制→認可拒否まで一通り確認する（詳細は各仕様書参照）。`.github/workflows/ci.yml`の
`playground-e2e`ジョブとしてpush/PRのたびに自動実行もされる（GitHub-hosted
runnerでも実際に`/dev/kvm`が使え、実Firecracker起動まで確認できることを確認済み）。
CLIを直接使う場合は
`go run ./cmd/kyuusha ... -addr=localhost:8080 -token=$KYUUSHA_TOKEN`
（`kyuusha token mint`で開発用トークンを発行）。VMのシリアルコンソールは
`kyuusha vm console -tenant=... -id=... [-follow]`で確認できる
（[Firecracker起動仕様](../docs/specs/firecracker-boot.md)参照）。

## 構成

kyuusha自身のサービス（api-gateway/compute/identity/image/network/block-storage/
compute-agent/NATS）は[システム構成仕様](../docs/specs/system-overview.md)を参照。
ここではplayground固有のobservabilityコンポーネントのみ挙げる。

| compose service | 実行イメージ | 備考 |
|---|---|---|
| `jaeger` | 公式`jaegertracing/all-in-one` | トレース収集・UI。`16686`をホストへ公開 |
| `prometheus` | 公式`prom/prometheus` | `playground/prometheus.yml`をマウント。`9090`をホストへ公開 |
| `grafana` | 公式`grafana/grafana` | `playground/grafana/provisioning`をマウント。`3000`をホストへ公開、匿名admin有効 |
| `loki` | 公式`grafana/loki` | ログ集約。ホストにポート非公開 |
| `promtail` | 公式`grafana/promtail` | Dockerソケットをマウントし全コンテナのログを収集、Lokiへpush |
| `image-assets` | `docker/Dockerfile`の`image-assets-server`ステージ | playground用のFirecracker kernel/rootfsを配信する静的ファイルサーバ。ホストにポート非公開（[Firecracker起動仕様](../docs/specs/firecracker-boot.md)参照） |

## Observability

| UI | URL | 用途 |
|---|---|---|
| Jaeger | http://localhost:16686 | トレース検索。`service`を`compute`/`compute-agent`/`identity`/`api-gateway`で絞り込む |
| Prometheus | http://localhost:9090 | メトリクス生クエリ（`rpc_server_call_duration_seconds_count`等） |
| Grafana | http://localhost:3000 | ダッシュボード（下記、全て自動プロビジョニング済み、ログイン不要）。Prometheus/Jaeger/Lokiがデータソースとして登録済み |

Grafanaのダッシュボードは`playground/grafana/provisioning/dashboards/json/*.json`として
リポジトリにコミット済みで、`docker compose up`のたびに毎回自動プロビジョニングされる
（Grafana自身のDBには何も持たせていない——コンテナを作り直しても消えない）。

| ダッシュボード | 内容 |
|---|---|
| `kyuusha overview` | 全サービス横断のサマリ（gRPCリクエストレート/エラーレート/p95レイテンシ、goroutine数、常駐メモリ） |
| `kyuusha: api-gateway` | api-gateway単体 |
| `kyuusha: identity` | identity単体 |
| `kyuusha: image` | image単体 |
| `kyuusha: compute` | compute + compute-reconciler（reconcileループ分離後の2プロセスをまとめて表示） |
| `kyuusha: network` | network + network-reconciler |
| `kyuusha: block-storage` | block-storage + block-storage-reconciler |
| `kyuusha: compute-agent` | compute-agentのgRPCクライアント呼び出し（compute-agent自身はgRPCサーバーを持たない）に加え、`/metrics/resources`のVM CPU/メモリ使用量（[メトリクス仕様](../docs/specs/observability-metrics.md)「/metrics/resources」参照） |
| `kyuusha: fleet` | リソース件数（Tenant/Image/VM/Volume/VolumeAttachment/Subnet/NetworkInterface、tenant_id×phase別）とQuota使用率（[メトリクス仕様](../docs/specs/observability-metrics.md)「リソース件数・Quota使用状況」参照） |
| `kyuusha: virtual machines` | VM単体に絞った集約ビュー（件数・tenant別内訳・`$tenant`/`$vm_id`で絞り込めるVM CPU/メモリ/ディスクI/O、NetworkInterfaceスループット、Volume IOPS/スループット（ブロックデバイスのみ）） |
| `kyuusha: logs` | Lokiのログ量・エラー率・ライブログ（`compose_service`フィルタ付き） |
| `kyuusha: audit` | 監査ログ専用（「誰が・何をしたか」、[監査ログ仕様](../docs/specs/audit-logging.md)参照） |

ログ（監査ログ含む）はPromtailがDockerソケット経由で全コンテナから収集しLokiへpushする。
GrafanaのExplore（データソース: Loki）で以下のようなLogQLクエリが使える。

```
{compose_service="api-gateway"} | json | audit="true"
```

監査ログのフィールド（`event`/`tenant_id`/`sub`/`trace_id`等）は
[監査ログ仕様](../docs/specs/audit-logging.md)を参照。`sub`（誰が）をトークンに乗せたい場合は
`kyuusha token mint -sub=alice@example.com`のように指定する。

詳細は[トレーシング仕様](../docs/specs/observability-tracing.md)・
[メトリクス仕様](../docs/specs/observability-metrics.md)・
[監査ログ仕様](../docs/specs/audit-logging.md)を参照。

## 後片付け

```sh
docker compose -f playground/docker-compose.yml down
```
