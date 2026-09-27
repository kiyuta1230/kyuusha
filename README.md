# kyuusha

kyuushaは、「OpenStackを導入するには大きすぎる（運用チームを抱えられない）が、ハイパーバイザーが
数百台規模になるとVMwareのライセンスコストが厳しくなる」という間に落ちる企業向けの、小さな
IaaS。KaaS(Kubernetes as a Service)クラスタへハイパーバイザーを供給することだけに機能を絞り、
VMを最終利用者が長期間使う「ペット」として扱う前提を持たない——OpenStack(Nova/Neutron/Cinder)
の縮小版と言うと早い。

想定スケールはハイパーバイザー〜500台・VM〜1〜2万台・テナント(KaaSクラスタ)〜500。
**なぜこの設計なのか**は
[docs/why-kyuusha.md](docs/why-kyuusha.md)に、詳しい設計判断の経緯は
[docs/architecture.md](docs/architecture.md)を参照。

> **ステータス**: 開発中。ストレージ以外の主要機能（VM/Hypervisor/Image/Network/CLI/認証認可/
> 東西mTLS/Hypervisor bootstrapトークン/cgroupリソース制限）はplayground環境で一通り
> 動作確認済み。VMMドライバはFirecracker/cloud-hypervisorの両方が実際にVMを起動する
> （[起動方式の比較](docs/specs/cloud-hypervisor-boot.md)）。Firecrackerは実jailer
> （chroot+uid/gid降格）でラップ済み、cloud-hypervisorは静的バイナリ+組み込みseccompで
> 外部jailer自体が不要（詳細は各仕様書を参照）。実ブロックストレージバックエンド
> （ZFS/NVMe-oF等）は未実装で、Volumeは既存の実ファイル/デバイスへの参照+非同期検証のみ
> （kyuusha自身はプロビジョニングしない、詳細は[Volume仕様](docs/specs/volume.md)参照）。

## 構成

| コンポーネント | 役割 |
|---|---|
| `api-gateway` | clientが到達できる唯一の公開エンドポイント。JWT検証＋OPA認可 |
| `identity` | Tenant（テナント・Quota上限値）管理 |
| `compute` | VirtualMachine・Hypervisor管理。スケジューラ、Quota強制、Image/Network検証 |
| `compute-agent` | 各ハイパーバイザー上で動くagent。実VMM（Firecracker/cloud-hypervisorどちらも）を起動する |
| `image` | Image（外部URL参照+digest）管理。テナント間共有（PUBLIC/PRIVATE）対応 |
| `network` | Subnet・NetworkInterface管理。VLAN/IPアドレス払い出し(IPAM) |
| `block-storage` | Volume・VolumeAttachment管理（実バックエンドはまだ無い） |
| `kyuusha`（CLI） | 上記すべてをapi-gateway経由で操作するクライアント |

詳細は[システム構成仕様](docs/specs/system-overview.md)を参照。

## クイックスタート（playground）

`/dev/kvm`があれば実VMM（Firecracker/cloud-hypervisor）までVMを起動する、multi-hypervisorのローカル環境。

```sh
docker compose -f playground/docker-compose.yml up -d --build
./playground/scenario.sh
```

`scenario.sh`がTenant/Image/VM作成からスケジューリング・Quota強制・認可拒否まで一通り確認する。
CLIを直接使う場合:

```sh
export KYUUSHA_TOKEN="$(go run ./cmd/kyuusha token mint -tenant=<tenant-id>)"
go run ./cmd/kyuusha vm list -addr=localhost:8080 -tenant=<tenant-id>
```

Observability UI（Jaeger/Prometheus/Grafana）や後片付け手順は
[playground/README.md](playground/README.md)を参照。

## 開発

```sh
go build ./...
go test ./...
buf lint && buf generate   # proto/ 配下を変更した場合
```

protoの生成コードは`gen/go/`にコミット済み（`buf generate`の再実行が必要なのはprotoを
変更したときのみ）。

## ドキュメント

- [docs/why-kyuusha.md](docs/why-kyuusha.md) — なぜこの設計か（OpenStack/Harvester/Flintlock/KubeVirtとの比較）
- [docs/architecture.md](docs/architecture.md) — 設計判断の経緯・議論・トレードオフ
- [docs/specs/](docs/specs/README.md) — 完成した機能単位の現状仕様
- [docs/release-notes.md](docs/release-notes.md) — 日付付きの変更履歴
- [docs/open-questions.md](docs/open-questions.md) — 未決事項
- [docs/rolling-upgrade.md](docs/rolling-upgrade.md) — コントロールプレーンのローリングアップグレード手順
