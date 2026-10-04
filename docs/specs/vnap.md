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
  `subnet_id`/`zone`/`subnet_labels`/`mac_address`/`ip_address`/`subnet_cidr`/`prefix_len`/
  `gateway_ip`/`vlan_id`/`primary`——`subnet_id`/`zone`があるので、プラグインは
  受け取ったtapがどのSubnetのものかを(zone, vlan_id)から逆引きする必要が無い。
  `subnet_labels`はSubnetの`meta.labels`（[外部システム連携仕様](external-integration.md)
  「ラベルとアノテーション」）の、VMがスケジュールされた時点のスナップショット——
  その後にSubnetのラベルを変えても、稼働中のVMのattachは呼び直されない。
  プラグインは知らないフィールドを無視すること（フィールドは今後も追加のみで増える）
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
  加えることで、組み込み実装には無い**実際のホスト跨ぎL2疎通**を実現する。
  **純粋なL2の延伸に徹し、ブリッジに`gateway_ip`を付けない**（組み込み実装との違い）
  ——Subnetのgatewayはファブリック側（テナントを分離するVRFの中のleaf/ToRのSVI）が持つ。
  全ホストのブリッジにも同じ`gateway_ip`を付けると、同じVLAN上で全ホストとSVIが
  同じIPを別々のMACで名乗ってARPを奪い合い、勝ったホストがファブリックのVRFを
  迂回してルーティングしてしまうため。他の
  参考実装と違い、どのNICがアップリンクかというホストレベルの設定を、VNAP payloadでは
  なく環境変数`VNAP_UPLINK_IFACE`（compute-agentプロセスから継承）で受け取る——
  これはFRR設定のようなプロトコルレベルの環境依存が無く、`frr-ipv4-unicast.sh`/
  `frr-vrf-host-route.sh`より「そのまま使える」度合いが高い参考実装
  （[network-deployment-guide.md](../network-deployment-guide.md)
  「1. VLANプール設計」参照）。containerlab製のleaf-spine-leaf CLOS疑似ファブリック
  （本物のVLAN-aware Linuxブリッジをスイッチ役に見立てた4ホップ構成）でこのスクリプト
  自体をそのまま実行し、実機確認済み（ホスト跨ぎのVM間疎通に加え、leaf1のSVIを
  gatewayとしてSubnet外へ出られること、gatewayへのARPに応答するのがSVIだけであること。
  詳細はdocs/release-notes.md参照）
- `examples/vnap-plugins/frr-ipv4-unicast.sh`——pure L3・IP一意
  （[network-deployment-guide.md](../network-deployment-guide.md)
  「3.5. Pure L3デプロイの場合」参照）デプロイ向けのサンプル。VRFを一切使わず、
  VMごとのtapへ`gateway_ip`をSubnetの実prefix長（`/32`ではない）で直接付与して
  ハイパーバイザ自身を本物のL3ゲートウェイにし、proxy ARPを有効化した上で、
  VM自身のIPを`/32`のstatic routeとしてFRRのデフォルトルーティングインスタンスへ
  注入する。ハイパーバイザ自身もLeafとL3で接続しデフォルトルートを受け取る構成を
  前提にする（テナント間のIPアドレス空間がfabric全体で重複しないことが大前提——
  重複しうる場合は`frr-vrf-host-route.sh`を使うこと）。`playground/ipv4-unicast-clos/`
  （containerlab製、本物のFRRがleaf-spine-leafのスイッチ役、unnumbered eBGP、
  VRF/EVPN/VXLANは一切無し）でホスト跨ぎの実機確認を行った
- `examples/vnap-plugins/frr-vrf-host-route.sh`——pure L3・IP重複許容
  （[network-deployment-guide.md](../network-deployment-guide.md)
  「3.5. Pure L3デプロイの場合」参照）デプロイ向けのサンプル。共有ブリッジを使わず、
  VMごとのtapへ`gateway_ip`を`/32`で直接付与しproxy ARPを有効化した上で、VM自身の
  IPを`/32`のホストルートとしてテナントのVRF内のFRR static route（`vtysh`経由）へ
  注入する。単一Hypervisorローカルの確認（Firecrackerゲストがブリッジ無しで実際に
  起動しゲスト自身がgatewayへのpingに成功、VM削除時にFRR側のルートも正しく
  引き上げられることを確認）に加え、ホスト跨ぎの実機確認を2通りの網側実現方式——
  `playground/evpn-vxlan-clos/`（本物のBGP EVPN Type-5・VXLANカプセル化あり）と
  `playground/vrf-lite-clos/`（EVPN/VXLAN無し、VRFスコープの素の`ipv4 unicast`
  eBGPのみ）——の両方で行った。**このスクリプト自身のattach/detachロジックは
  網側がどちらであっても完全に同一**——FRRのRIBへVRFスコープのstatic routeを
  出し入れするだけで、その先をEVPNが運ぶかプレーンなBGPが運ぶかは関知しない設計の
  帰結。`playground/evpn-vxlan-clos/`での検証で**tapをVRFへ`master`として所属
  させる処理が漏れていたバグ**（注入したstatic routeが常にno-opになっていた）を
  発見・修正した（詳細はスクリプト自身のコメントとdocs/release-notes.md参照）。
  BGP/EVPN/VRFの設定自体はASN方式・numbered/unnumbered等が環境ごとに大きく異なる
  プロトコルレベルの事情を抱えるため、`vlan-trunk.sh`と違い「そのまま使える」
  参考実装にはなり得ず、読んで自分の環境に合わせて作り込む前提のまま
