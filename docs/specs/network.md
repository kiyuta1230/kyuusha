# network仕様

## 概要

`network`サービスは`Subnet`・`NetworkInterface`を管理するCRUD+Watchサービス
（設計は`docs/architecture.md`「networkサービスのリソース: Subnet / NetworkInterface」参照）。
**このドキュメントが書く現状は、そのAPIの型と最小限のバリデーションのみが実装済みの段階**で、
実際のVLAN ID払い出し（IPAM）・IPアドレス割当・tap配線・compute-agentとの連携は
まだない。computeが最初にCRUD+Watchだけの状態から始まり、後にスケジューラ・Quota・
Firecracker起動を順に足していったのと同じ進め方。

## リソース

`Subnet`はテナント(KaaSクラスタ)が持つ1つ以上のネットワーク区画で、それぞれ独立してVLAN IDを
持つ。`NetworkInterface`はVirtualMachineとSubnetの結びつきを表す一時的なリソース
（`VolumeAttachment`と同じ「結びつきそのものをリソースにする」パターン）。フィールドの詳細は
proto（`proto/kyuusha/network/v1/subnet.proto`・`networkinterface.proto`）参照。

## モックの範囲

| 項目 | 現状 | 実装後（IPAM、次のフェーズ） |
|---|---|---|
| `Subnet.status.vlan_id` | Create時に即Ready、グローバルな連番をそのまま返すだけ（`spec.zone`は見ない） | zoneごとに独立したVLAN IDプールから排他的に払い出し（`docs/architecture.md`参照） |
| `NetworkInterface.status.ip_address` | 常に`"0.0.0.0"` | `Subnet.spec.cidr`内から実際に割り当て |
| `NetworkInterface.status.mac_address` | 連番から生成した仮のMAC | 実装が変わる可能性は低い（そのままでも良い） |
| `NetworkInterface.status.hypervisor` | 常に空文字列 | tap配線が完了したハイパーバイザーを反映 |

`NetworkInterface.Create`は`spec.subnet_id`が指す`Subnet`が存在し、同じテナントに属し、
`Ready`であることは検証する（存在しない/他テナント/未Readyなら`ErrValidation`）。これは
「参照先が存在しない・使えない状態のリソースを作らない」という、computeのImage検証
（[Image仕様](image.md)参照）と同じ設計原則。

## この実装がカバーしないもの

- **IPAM**（次のフェーズ。上表参照）
- **tap配線・VLANタグ付け**: 実装されてもnetwork-agentという別プロセスは作らない方針
  ——compute-agentに統合する。理由は、tap配線がVM起動と同じ物理ホスト内で完結する処理で
  あり、OpenStackのnova-compute/neutron-agent分離のような**プロセス間の往復調整**
  （ポートbind要求→plugged eventの待ち合わせ）を持ち込む必要がないため
- **compute側の統合**: VM Create時に`spec.network_interfaces`からNetworkInterfaceを
  作る/参照する連携はまだない。今のcomputeの`VirtualMachineSpec.network_interfaces`
  フィールド自体は存在するが、networkサービスへの問い合わせはしていない
- ネットワーク分離の実現方式（VRF/ルートリーク禁止によるテナント間非疎通性、
  DNS/名前解決の拡張機能）は設計のみ（`docs/architecture.md`参照）、実装はまだ

## エンドポイント

`network :8084`（`SubnetService`, `NetworkInterfaceService`）。api-gateway経由でのみ
到達可能（[システム構成仕様](system-overview.md)参照）。CLIは[CLI仕様](cli.md)参照。
