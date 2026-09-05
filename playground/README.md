# Playground

`docker-compose.yml`（リポジトリルート）が起動する、ローカルの多ハイパーバイザー環境。

## 起動

```sh
docker compose up -d --build
```

## 動作確認

```sh
./playground/scenario.sh
```

Tenant作成→VM作成→スケジュール→Quota強制→認可拒否まで一通り確認する（詳細は各仕様書参照）。
CLIを直接使う場合は `go run ./cmd/kyuusha ... -addr=localhost:8080 -token=$KYUUSHA_TOKEN`
（`kyuusha token mint`で開発用トークンを発行）。

## Observability

| UI | URL | 用途 |
|---|---|---|
| Jaeger | http://localhost:16686 | トレース検索。`service`を`compute`/`compute-agent`/`identity`/`api-gateway`で絞り込む |
| Prometheus | http://localhost:9090 | メトリクス生クエリ（`rpc_server_call_duration_seconds_count`等） |
| Grafana | http://localhost:3000 | ダッシュボード（`kyuusha overview`が自動プロビジョニング済み、ログイン不要） |

詳細は[トレーシング仕様](../docs/specs/observability-tracing.md)・
[メトリクス仕様](../docs/specs/observability-metrics.md)を参照。

## 後片付け

```sh
docker compose down
```
