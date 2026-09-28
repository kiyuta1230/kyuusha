# VNAP（VM Network Attach Protocol）仕様

`docs/architecture.md`「VMのネットワーク接続をCNIのようにプラガブルにすべきか」
（解決済みリスト参照）で確定した設計の実装。ローカルなtap配線プラグイン契約——
`-network-attach-bin`（compute-agentのフラグ）を指定すると、[network仕様](network.md)
「tap配線とローカルネットワーク」の**スイッチへの実配線ステップだけ**を外部バイナリへ
委譲できる——tapデバイス自体の作成・削除は常に`netsetup`（厩舎自身）が担う。未指定
（既定）なら今まで通り固定のLinuxブリッジ実装のまま、既存の挙動は一切変わらない。

- **CNI互換ではない**: VMのtapデバイスに対応するnetnsは存在しないため、CNIの
  `CNI_NETNS`/`CNI_IFNAME`のようなnetns移動の契約は採用しない
  （`docs/architecture.md`「設計原則: KubeVirtを反面教師にする」節が名指す誤りを
  繰り返さないため）。CNIから借りているのは「バイナリ+stdin JSON+exit code」
  という呼び出し規約パターンだけ
- **呼び出し**: `<bin> attach`/`<bin> detach`をexec、標準入力にJSON
  （`internal/compute-agent/netsetup`の`pluginRequest`）を渡す。ADD/DELという
  CNI用語は使わない
- **attachのpayload**（全フィールド）: `tap_name`/`iface_id`/`vm_id`/`tenant_id`/
  `mac_address`/`ip_address`/`prefix_len`/`gateway_ip`/`vlan_id`/`primary`
- **detachのpayload**（識別に要る最小限のみ）: `tap_name`/`iface_id`/`vm_id`/
  `tenant_id`——ポートを消すのに以前の設定内容（IP/MAC/VLAN等）は不要なため
- **成否はexit codeのみ**（0=成功）。構造化されたResult JSONは要求しない——
  tap/IP/MACは全て厩舎が既に作成済みで、プラグインが新たに報告すべき情報が無いため
- **タイムアウト**: 10秒（`pluginTimeout`）。ハングしたプラグインがVM起動/削除を
  無期限にブロックしないようにする
- **冪等性**: attach/detachはプラグイン側の責務として冪等でなければならない
  （stuck-phase retry sweepがCreateCommandを再送すると`Wire`も再実行されるため）
- **失敗時の扱い**: attach失敗は`Wire`の既存のエラー経路にそのまま乗る（Boot全体が
  失敗し、それまでに配線済みのtapは既存の`cleanup()`が後始末）。detach失敗は
  ログのみで継続——tapデバイス自体は、detachプラグインの成否に関わらず必ず削除される
  （プラグイン障害でtapがリークすることはない）

**参考実装**:

- `examples/vnap-plugins/vlan-trunk.sh`——VLANトランク（Type-2、本ガイドの既定の前提）
  デプロイ向け。組み込みのLinuxブリッジ実装（`kbr<vlan_id>`）と同じ配線に加えて、
  アップリンクNICへ802.1Qタグ付きVLANサブインターフェースを作成しそのブリッジへ
  加えることで、組み込み実装には無い**実際のホスト跨ぎL2疎通**を実現する。他の
  参考実装と違い、どのNICがアップリンクかというホストレベルの設定を、VNAP payloadでは
  なく環境変数`VNAP_UPLINK_IFACE`（compute-agentプロセスから継承）で受け取る——
  これはFRR設定のようなプロトコルレベルの環境依存が無く、`frr-type5.sh`より
  「そのまま使える」度合いが高い参考実装（[network-deployment-guide.md](../network-deployment-guide.md)
  「1. VLANプール設計」参照）。containerlab製のleaf-spine-leaf CLOS疑似ファブリック
  （本物のVLAN-aware Linuxブリッジをスイッチ役に見立てた4ホップ構成）でこのスクリプト
  自体をそのまま実行し、実機確認済み（詳細はdocs/release-notes.md参照）
- `examples/vnap-plugins/frr-type5.sh`——EVPN Type-5（pure L3）
  デプロイ向けのサンプル（[network-deployment-guide.md](../network-deployment-guide.md)
  「3.5. Type-5（EVPN pure L3）デプロイの場合」参照）。共有ブリッジを使わず、
  VMごとのtapへ`gateway_ip`を`/32`で直接付与しproxy ARPを有効化した上で、VM自身の
  IPを`/32`のホストルートとしてカーネルとFRR（`vtysh`経由）の両方へ注入する。単一
  Hypervisorローカルの確認（Firecrackerゲストがブリッジ無しで実際に起動しゲスト自身が
  gatewayへのpingに成功、VM削除時にFRR側のルートも正しく引き上げられることを確認）に
  加え、`playground/frr-type5-clos/`（containerlab製、本物のFRRがleaf-spine-leafの
  スイッチ役を担う、BGP EVPN Type-5・VXLANカプセル化あり）でホスト跨ぎの実機確認も
  行った——この検証で**tapをVRFへ`master`として所属させる処理が漏れていたバグ**
  （注入したstatic routeが常にno-opになり、Type-5が実質機能しない状態だった）を
  発見・修正した（詳細はスクリプト自身のコメントとdocs/release-notes.md参照）。
  BGP/EVPNの設定自体はASN方式・numbered/unnumbered等が環境ごとに大きく異なる
  プロトコルレベルの事情を抱えるため、`vlan-trunk.sh`と違い「そのまま使える」
  参考実装にはなり得ず、読んで自分の環境に合わせて作り込む前提のまま
