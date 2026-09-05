# network仕様

## 概要

`network`サービスは`Subnet`・`NetworkInterface`を管理するCRUD+Watchサービス
（設計は`docs/architecture.md`「networkサービスのリソース: Subnet / NetworkInterface」参照）。
VLAN ID・IPアドレスの払い出し（IPAM）は実装済みだが、tap配線・compute-agentとの連携は
まだない。computeが最初にCRUD+Watchだけの状態から始まり、後にスケジューラ・Quota・
Firecracker起動を順に足していったのと同じ進め方で、このサービスも一段ずつ実体化させている。

## リソース

`Subnet`はテナント(KaaSクラスタ)が持つ1つ以上のネットワーク区画で、それぞれ独立してVLAN IDを
持つ。`NetworkInterface`はVirtualMachineとSubnetの結びつきを表す一時的なリソース
（`VolumeAttachment`と同じ「結びつきそのものをリソースにする」パターン）。フィールドの詳細は
proto（`proto/kyuusha/network/v1/subnet.proto`・`networkinterface.proto`）参照。

## IPAM（`internal/network/ipam.go`）

ハイパーバイザーagentは一切関与しない、network自身の中で完結する同期的なプール払い出し
（`docs/architecture.md`「VLAN IDの払い出し」参照）。

- **VLAN ID**: zoneごとに独立したプール（1〜4094。0と4095は予約のため対象外）。
  `Subnet.spec.zone`単位で排他的に払い出す。同じ番号を別zoneで再利用できる
- **IPアドレス**: `Subnet`ごとに、その`spec.cidr`の中から排他的に払い出す。ネットワーク
  アドレス・ブロードキャストアドレス・（設定されていれば）`spec.gateway_ip`は対象外。
  `/31`・`/32`（利用可能なホストアドレスが無い）や IPv6 CIDR は現状非対応で、常に
  「枯渇」として扱われる
- **`spec.allocatable_ip_ranges`**: 空なら上記の通りCIDR全体が対象。指定した場合は
  `["10.0.1.10-10.0.1.20", ...]`のように`<開始>-<終了>`形式のIPv4範囲だけが払い出し対象になる
  （例: 既存の静的割当や将来予約でCIDRの一部を空けておきたい場合）。Create時に各範囲が
  `spec.cidr`の外に出ていないかを検証する（`ErrValidation`）。範囲内であってもネットワーク
  アドレス・ブロードキャストアドレス・`gateway_ip`は常に除外される（範囲側でうっかり
  含めても無視されるだけで、エラーにはしない）
- **MACアドレス**: グローバルな連番から生成する簡易実装。枯渇しうる共有プールではないため
  IPAMとしての特別な設計は不要（今後もこのままで問題ない見込み）

### プール枯渇時の挙動

Quota（[Quota仕様](quota.md)参照）とは異なり、プール枯渇は**Createを拒否しない**——
リクエスト自体は正当で、他のリソースが削除されれば空きが出るかもしれないため、VMの
スケジュール失敗時と同じ考え方（[VMスケジュール仕様](vm-scheduling.md)参照）を採る。

- `Subnet`: `status.phase`が`Pending`のまま、`Condition{type: VlanPoolExhausted, status:
  true}`を報告する
- `NetworkInterface`: 同様に`Pending`のまま`Condition{type: IPPoolExhausted, status: true}`
  を報告する（`mac_address`は枯渇に関係なく即座に払い出し済み）
- 10秒間隔の定期スイープ（`Service.Run`）が全`Pending`のSubnet/NetworkInterfaceに対して
  再度払い出しを試みる（他のSubnet/NetworkInterfaceのDelete自体はPending中のものを
  再トリガーしないため、VMスケジュールの定期スイープと同じ理由で必要）
- 成功すると同じConditionが`status: false`に更新される（削除はされない）
- SubnetのDelete/NetworkInterfaceのDeleteは、`Ready`で実際に払い出し済みだった場合のみ
  VLAN ID/IPアドレスをプールへ返却する

## `spec.mesh_group`（宣言のみ、ACL強制はまだ）

同一テナントが複数のAZにまたがってSubnetを持つ場合（AZ毎に別Subnet/別VLANになる設計、
`docs/architecture.md`「マルチAZにまたがるVirtualMachineは作れない」参照）、AZ間の
**経路**はRoute Targetによる自動交換で疎通するが、`NetworkInterfaceSpec.ingress_rules`の
**ACL**はSubnet CIDR外を既定で拒否するため、AZ間で通信したい場合は本来Subnetの組み合わせ
ごとに手動でallowルールを書く必要がある。

`spec.mesh_group`は、この手間を減らすための**意図の宣言**フィールド（`shared_with_tenant_ids`
と同じ位置づけ）: 同一テナント内で同じ`mesh_group`値を持つSubnet同士は、デフォルトで
互いを許可する対象とみなす、という設計上の意図だけを表す。**現状これを実際に強制する
ACLエンジンはどこにも存在しない**（`ingress_rules`自体もまだ実際のホスト側ファイアウォール
に反映されていない、同じ段階）。tap配線・ACL適用が実装される時に、`mesh_group`が一致する
Subnetの組み合わせを自動許可する、という形で参照される想定。

## Create時のバリデーション

`NetworkInterface.Create`は`spec.subnet_id`が指す`Subnet`が存在し、同じテナントに属し、
`Ready`であることを検証する（存在しない/他テナント/未Readyなら`ErrValidation`）。これは
「参照先が存在しない・使えない状態のリソースを作らない」という、computeのImage検証
（[Image仕様](image.md)参照）と同じ設計原則。IPAM自体のプール枯渇は上記の通りCreateを
拒否しない（Pendingで受理する）ため、この検証とは別軸。

## この実装がカバーしないもの

- **tap配線・VLANタグ付け**: 実装されてもnetwork-agentという別プロセスは作らない方針
  ——compute-agentに統合する。理由は、tap配線がVM起動と同じ物理ホスト内で完結する処理で
  あり、OpenStackのnova-compute/neutron-agent分離のような**プロセス間の往復調整**
  （ポートbind要求→plugged eventの待ち合わせ）を持ち込む必要がないため。`NetworkInterface.
  status.hypervisor`は現状常に空文字列で、tap配線が実装された時に反映される。この配線を
  CNIのように任意バイナリへ委譲するプラガブルな仕組みにすべきかは判断保留中
  （`docs/architecture.md`「未決事項」3.参照）
- **compute側の統合**: VM Create時に`spec.network_interfaces`からNetworkInterfaceを
  作る/参照する連携はまだない。今のcomputeの`VirtualMachineSpec.network_interfaces`
  フィールド自体は存在するが、networkサービスへの問い合わせはしていない
- ネットワーク分離の実現方式（VRF/ルートリーク禁止によるテナント間非疎通性、
  DNS/名前解決の拡張機能）は設計のみ（`docs/architecture.md`参照）、実装はまだ

## エンドポイント

`network :8084`（`SubnetService`, `NetworkInterfaceService`）。api-gateway経由でのみ
到達可能（[システム構成仕様](system-overview.md)参照）。CLIは[CLI仕様](cli.md)参照。
