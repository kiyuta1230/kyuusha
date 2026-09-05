# Playground

`docker-compose.yml`（このディレクトリ）が起動する、ローカルの多ハイパーバイザー環境。
リポジトリルートから実行する。

## 起動

```sh
docker compose -f playground/docker-compose.yml up -d --build
```

## 動作確認

```sh
./playground/scenario.sh
```

Tenant作成→Image作成→Ready待ち→VM作成→スケジュール→Quota強制→認可拒否まで一通り確認する
（詳細は各仕様書参照）。CLIを直接使う場合は
`go run ./cmd/kyuusha ... -addr=localhost:8080 -token=$KYUUSHA_TOKEN`
（`kyuusha token mint`で開発用トークンを発行）。

## 構成

kyuusha自身のサービス（api-gateway/compute/identity/image/compute-agent/NATS）は
[システム構成仕様](../docs/specs/system-overview.md)を参照。ここではplayground固有の
observabilityコンポーネントのみ挙げる。

| compose service | 実行イメージ | 備考 |
|---|---|---|
| `jaeger` | 公式`jaegertracing/all-in-one` | トレース収集・UI。`16686`をホストへ公開 |
| `prometheus` | 公式`prom/prometheus` | `playground/prometheus.yml`をマウント。`9090`をホストへ公開 |
| `grafana` | 公式`grafana/grafana` | `playground/grafana/provisioning`をマウント。`3000`をホストへ公開、匿名admin有効 |
| `loki` | 公式`grafana/loki` | ログ集約。ホストにポート非公開 |
| `promtail` | 公式`grafana/promtail` | Dockerソケットをマウントし全コンテナのログを収集、Lokiへpush |

## Observability

| UI | URL | 用途 |
|---|---|---|
| Jaeger | http://localhost:16686 | トレース検索。`service`を`compute`/`compute-agent`/`identity`/`api-gateway`で絞り込む |
| Prometheus | http://localhost:9090 | メトリクス生クエリ（`rpc_server_call_duration_seconds_count`等） |
| Grafana | http://localhost:3000 | ダッシュボード（`kyuusha overview`が自動プロビジョニング済み、ログイン不要）。Prometheus/Jaeger/Lokiがデータソースとして登録済み |

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
