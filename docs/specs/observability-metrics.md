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
`/metrics/resources`として**払い出したVirtualMachineの利用状況**（cgroup由来のCPU/
メモリ）を公開する（2026-09-13実装、`docs/architecture.md`「払い出したリソース自身の
メトリクス」参照）。実装は`internal/compute-agent/resourcemetrics.Collector`という
独立の`prometheus.Collector`で、`/metrics`とは別の`prometheus.Registry`から配信する
（`resource_version`/Watchに一切混ぜないという設計上の理由——同セクション参照）。

| メトリクス | 種別 | 主なラベル | 取得元 |
|---|---|---|---|
| `kyuusha_vm_cpu_usage_seconds_total` | Counter | `hypervisor`, `tenant_id`, `vm_id` | cgroup v2 `cpu.stat`の`usage_usec` |
| `kyuusha_vm_memory_usage_bytes` | Gauge | 同上 | cgroup v2 `memory.current` |
| `kyuusha_vm_memory_limit_bytes` | Gauge | 同上 | cgroup v2 `memory.max`（`spec.memory_mb`と一致。cgroupが無制限（`"max"`）を報告する場合は出力されない） |

収集はスクレイプの都度、その場でcgroup統計ファイルを読むだけ（バックグラウンドの
ポーリングループやキャッシュ状態は持たない）。`internal/compute-agent/cgroup.Apply`が
best-effortである（cgroup v2委譲の無いホストではVMは無制限のまま起動を続ける）のと
同じ理由で、cgroupが適用されなかったVMはこのエンドポイントに単に現れない
（エラーにもならない）。

**未実装（今回のスコープ外）**: NetworkInterface（tapデバイスのネットワークI/O）と
Volume/VolumeAttachment（ストレージノード側のIOPS/スループット）は、
`docs/architecture.md`の同セクションが元々挙げていた3種のうちまだ手が付いていない
——それぞれ`network-agent`側・block-storage側の実装が別途必要。

## playgroundでの可視化

playgroundでは`playground/grafana/provisioning/dashboards/json/kyuusha-overview.json`として
`kyuusha overview`ダッシュボードを自動プロビジョニングしており、以下のパネルがある
（構成の詳細は[playground/README.md](../../playground/README.md)参照）。

- gRPCリクエストレート（service/method別）
- gRPCエラーレート（`rpc_response_status_code != OK`）
- gRPCサーバーp95レイテンシ（service別）
- goroutine数（service別）
- 常駐メモリ（service別）
