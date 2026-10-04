# kyuusha ネットワーク運用ガイド（デプロイ前提条件）

## 対象読者

このドキュメントは、**kyuushaを構成するソフトウェア（compute/network/block-storage等）を
実装・運用するチームではなく、物理ネットワークファブリック（CLOSファブリック、ToR/leafスイッチ、
EVPN-VXLAN、VRF）を構築・運用するネットワークチーム**を対象にしている。

kyuushaは「薄い制御プレーンに徹し、物理ネットワークのオーケストレーションは行わない」という
設計方針を取っている（詳細は`docs/architecture.md`の「設計原則」「インフラ要件」各節を参照）。
そのため、**kyuushaが正しく動作するためにはこのドキュメントに書かれた前提条件が物理ファブリック側で
満たされている必要がある**。ここに書かれた設定はkyuusha自身が行わず、ネットワークチームが
別途構築する。

## 全体像

```
                    ┌─────────────────────────────────────┐
                    │        Spine層（L3ルーテッド）         │
                    └───────────────┬───────────────────────┘
                                     │
        ┌────────────────────────────┼────────────────────────────┐
        │ AZ-A                       │                    AZ-B     │
   ┌────┴─────┐                 ┌────┴─────┐    （L2は非接続。同一NetworkのRTのみBGP/EVPNで
   │ leaf/ToR │  EVPN-VXLAN      │ leaf/ToR │     Type-5ルート交換、詳細は2.5節）
   │ (VTEP)   │← L2ストレッチ →  │ (VTEP)   │
   └────┬─────┘  (AZ内のみ)      └────┬─────┘
        │ 802.1Qトランク                │
   ┌──────────────┐               ┌──────────────┐
   │compute       │               │compute       │
   │hypervisor(s) │               │hypervisor(s) │
   └──────────────┘               └──────────────┘
```

- EVPN-VXLANによるL2ストレッチは**AZ内のファブリックに限定**する。AZを跨いでは意図的に繋げない
  （AZは電源系統・ネットワークファブリックが独立した障害ドメインという前提のため）
- ただし**同一NetworkのAZ間到達性は必須機能**（後述のRoute Targetによるルーティング）。
  「AZ間ルーティング全般が非ゴール」ではない点に注意（後述）
- 1つのkyuushaデプロイ＝1リージョン相当。マルチリージョンは別デプロイを立てて連携する話であり、
  このドキュメントのスコープ外

### VLAN到達範囲はアンダーレイ構成に依存する

kyuusha自身はVLAN IDを「Subnetごとに排他的なローカルな帳簿番号」としてしか扱わず、それが
実際にどこまで届くか（＝そのSubnetを使えるハイパーバイザの範囲）は物理ファブリックの構成
そのもの次第——kyuushaのスケジューラは現状「どのハイパーバイザがどのVLANへ到達できるか」を
一切考慮せず、あるSubnetのVMは（zone/driver/容量等の通常のフィルタさえ満たせば）
そのAZ内のどのハイパーバイザへでもスケジュールされうる、という前提で動く。つまり
**そのSubnetのVLANがスケジュール候補になりうる全ハイパーバイザへ実際に届いていることは、
kyuusha側では一切保証されない、ネットワークチーム側の前提条件**になる。
ネットワークチームがどの構成を選ぶかで、実際の到達範囲は大きく変わる:

**(a) 伝統的なコア/ToR構成**（コアスイッチ配下でVLANを広くトランク）

```
        ┌─────────────────────────────────────┐
        │      Core switch（全VLANをトランク）     │
        └───────┬──────────────────┬────────────┘
                 │ トランク            │ トランク
           ┌─────┴─────┐       ┌─────┴─────┐
           │  ToR #1   │       │  ToR #2   │
           └─────┬─────┘       └─────┬─────┘
      ┌──────────┴──────────┐┌───────┴──────────────┐
      │compute hypervisor(s) ││compute hypervisor(s) │
      └──────────────────────┘└──────────────────────┘
```

コア配下の全ToRでVLANが流れていれば、そこに繋がる全ハイパーバイザで同じVM networkが
使える——到達範囲はコアスイッチ配下全体に事実上一致する。

**(b) CLOS、オーバーレイ無し**（VXLAN等を使わない素のスパイン-リーフ）

```
        ┌──────────────────┐
        │   Spine（L3のみ）   │
        └────┬────────┬─────┘
             │L3        │L3
        ┌────┴───┐ ┌───┴────┐
        │ Leaf#1 │ │ Leaf#2 │  ← Leaf単位でL2(VLAN)が独立
        │ (ToR)  │ │ (ToR)  │
        └───┬────┘ └───┬────┘
      ┌──────┴──────┐┌──────┴──────┐
      │compute       ││compute       │
      │hypervisor(s) ││hypervisor(s) │
      └──────────────┘└──────────────┘
```

Spine-Leaf間はL3ルーテッドのみでVLANを中継しないため、L2(VLAN)はLeafごとに独立する。
同じVLAN番号を複数のLeaf配下で使っても、Leafを跨いだ瞬間に別のL2ドメイン扱いになり
到達しない——**VM networkは必然的にラック（そのLeaf配下）単位に閉じる**。VMの配置先を
同じラック内に収める、というスケジューリング側の制約が無い限り、この構成は事実上使えない

**(c) CLOS + VXLAN（LeafがVTEP）**——本ガイドが既定で前提にしている構成（上記「全体像」参照）

EVPN-VXLANでLeafがVTEPとしてL2をストレッチするため、あるVLAN(=VNI)の到達範囲は
**そのVXLAN/EVPNの設計次第**——「全体像」の図のようにAZ全体でストレッチする設計も、
一部のLeafだけをそのVNIのメンバーにする狭い設計も、ネットワークチーム側で自由に選べる。
本ガイドの「1. VLANプール設計」以降は、この(c)を、しかも**AZ全体でストレッチする**という
最も広い設計を前提に書かれている——(a)や(b)を採用する場合はその前提が変わる点に注意
（(b)を採用したい場合は「3.5. Pure L3デプロイの場合」節も参照: そもそもVLANの
L2ストレッチに頼らない別の実現方式がある）

## 1. VLANとアドレスの払い出し設計

kyuushaでは、Subnetに何を払い出すかを管理者が**NetworkClass**と**AllocationPool**で定義する
（[network仕様](specs/network.md)「リソース」参照。kyuusha自身はVLANという概念を知らず、
払い出された名前付きの値をVNAPプラグインへ渡すだけ）。VLANトランク方式（Type-2、本ガイドの
既定）での典型的な流れ:

1. **ネットワークチームが、AZごとにCIDRとVLAN IDの組を事前に決め、スイッチに設定する**
   （VLANのトランク、Network単位のVRFへのSVIの所属、SVIへのgateway_ipの設定）
2. その組を**組の一覧（`entries`）のAllocationPool**としてAZごとに登録する（例: `{key:
   "vlan-300", values: {"vlan_id": 300}, addresses: [{cidr: "192.168.30.0/24", gateway_ip:
   "192.168.30.254"}]}`）。組は丸ごと1つのSubnetに払い出される
3. NetworkClassでzoneごとにそのプールを参照する（`subnet: {"az-a": [az-aの組のプール],
   "az-b": [...]}`）。Route Target等のNetwork単位の値は`network`側に整数プールで足す（2.5節）
4. テナントはそのClassでNetworkを作り、Subnetを「どのNetworkの、どのAZに」と依頼するだけで、
   VLAN IDとCIDRとgatewayがkyuushaから払い出される

CIDRをテナント自身に選ばせたい場合は、組の一覧の代わりに「AZごとのVLAN IDの整数プール」と
「利用者指定のCIDRプール」をClassで組み合わせる（この場合のgatewayもファブリック側の設定と
合わせる必要がある点に注意）。

- VLAN IDは**AZごとに独立**してよい（同じ番号を別AZで再利用してよい——AZごとに別のプールを
  Classに割り当てる）
- 1つのL2ファブリックで最大4094まで。VLAN方式ではSubnetを足すたびにVLANを1つ消費するので、
  Subnetの初期サイズを大きめにする、上限を大きく超える見込みのあるAZはPure L3デプロイへ
  切り替える（「3.5. Pure L3デプロイの場合」節）——kyuusha側にVXLANへの自動エスケープパスの
  ような機能は無い
- 払い出し済みの組が残っている間は、その組をプールから消したり範囲を狭めたりできない
  （kyuusha側で拒否される）
- host（compute hypervisor）向けのToRポートは、そのAZで使用されうる全VLAN IDを許可する**トランクポート**
  として設定する
- **払い出された値をワイヤ上の802.1Qタグとして出すかどうかは、host-ToR間の実配線を担う
  VNAPプラグイン（`-network-attach-bin`）次第**。組み込み実装（`-network-attach-bin`未指定）は
  ホスト内でSubnetごとのLinuxブリッジに繋ぐだけでタグ付けを一切行わない。
  `examples/vnap-plugins/vlan-trunk.sh`という参考VNAPプラグインが、`subnet_values.vlan_id`を
  タグとしてアップリンクNICへのVLANサブインターフェース作成を担う——ゼロから自作する
  必要はない（[VNAP仕様](specs/vnap.md)「参考実装」参照）。このときSubnetの
  `gateway_ip`は**ファブリック側（そのNetworkのVRFのSVI）に必ず設定する**——
  `vlan-trunk.sh`はハイパーバイザー側でgatewayを名乗らない（純粋なL2の延伸）。Pure L3デプロイでは
  事情が異なる——「3.5. Pure L3デプロイの場合」参照

## 2. VRF設計とルートリークポリシー（最重要）

**これがNetwork間（ひいてはテナント間）の分離を実際に担保する仕組みであり、最も重要な設定項目。**

- **1 Network = 1 VRF**（AZごとに1インスタンス）とする。kyuushaのNetworkはルーティング
  ドメイン兼分離の境界で、同じNetworkのSubnet同士は疎通し、別のNetwork同士は疎通しない
  （同じテナントの別のNetwork同士も）。ゲートウェイ（leaf/ToRスイッチ）側で、同じNetworkの
  Subnet（VLAN）のSVIを、そのNetworkのVRFへ所属させる。どのVLANがどのNetworkのものかは、
  Networkに払い出されたRoute Target等の値（2.5節）とSubnetの値をVNAP/運用の取り決めで対応付ける
- **Network間のデフォルトルートリークは行わない**。VRFはルーティングテーブルそのものを
  分離するため、明示的なルートリークを設定しない限りNetwork間に経路自体が存在しない
  （これがVLANタグそのものではなく、VRF分離こそが分離を実現する理由。VLANは
  L2ブロードキャストドメインを分けるだけで、L3の到達可能性はゲートウェイの設定次第である点に注意）
- VRFを跨ぐルートリークは、次節で挙げる**限定的な例外と、Network同士を意図してつなぐ場合のみ**
  許可する（ファブリック側でVRFが実現されている構成では、Network同士をつなぐのは
  ネットワークチームのルートリーク設定になる）

### 許可される例外的ルートリーク

1. **共有の外向きNATゲートウェイ**: インターネットへのegressを提供する共有NATゲートウェイへの
   経路のみ、制御された形で全VRFへリークする
2. **共有DNSリゾルバ**: kyuushaのnetworkサービスが権威DNSを兼ねる共有リゾルバへの経路を、
   全VRFへリークする（Subnetの`dns_servers`が指すIP）

上記以外の「とりあえず疎通させておく」ようなルートリークは、テナント分離の前提を壊すため
**絶対に行わないこと**。

※ Networkの`shared_with_tenant_ids`（[network仕様](specs/network.md)参照）は、ここで
言う意味でのVRF間ルートリークとは無関係——他テナントの`NetworkInterface`がそのNetworkへ
**直接attachする**（＝所有テナントのVRF/VLANへそのまま参加する）ことを許可する宣言で、
2つの別々のVRFを跨ぐ経路を作るものではない。よってこのフィールドに対応する特別な
ルートリーク設定は不要。

### 防御層（参考）

万が一ここでの設定ミスによりルートがリークしても、kyuusha側のNetworkInterfaceには
「同じNetworkの外からのトラフィックはデフォルト拒否」というホスト側ACL（nftables）が
デフォルトで入っている。これは二重の防御であり、正しいVRF設定を代替するものではない。

## 2.5. 同一NetworkのAZ間ルーティング（Route Target）

NetworkはAZをまたぎ、AZに閉じるSubnetを束ねる。**同一NetworkのAZ間到達性が無いとそもそも
クラスタとして機能しない**。BGP/EVPN L3VPNの標準機能である**Route Target (RT)** を使って実現する。

- 各Networkに、AZを跨いで一意な**Route Targetを1つ**割り当てる——**kyuushaが払い出す**。
  NetworkClassのNetwork単位の参照に整数プールを置く（例: `network: [{pool: rt-pool, name:
  route_target}]`）と、Networkの作成時にkyuushaがそこから一意な値を払い出し、Networkの
  `status.values.route_target`に記録してVNAPへ（`network_values`として）渡す。ネットワーク
  チームの役割は、RTに使ってよい番号範囲を決めてプールとして登録すること
- そのNetworkの全AZのVRFインスタンスで、このRTを共通してimport/exportするよう設定する
- 同じRTを持つVRF同士は、spine層を経由したBGP/EVPN Type-5の標準動作としてルートを自動交換する。
  **Networkごとに個別のルートリーク設定を人手で組む必要はない**
- 異なるNetworkは異なるRTを持つため、このRTベースの仕組みではデフォルトで疎通しない
  （分離は引き続き保たれる）
- **障害分離**: AZ間の中継経路（spine層でのRT間ルート交換）が落ちても、そのNetworkの
  AZ内トラフィックはそのAZのVRF内で問題なく動き続ける。失われるのはAZ間到達性のみ
- **Route Distinguisher（RD）をSubnetのVLAN ID単体から機械的に導出しないこと**: VLAN IDは
  AZ内でのみ一意（AZを跨いで同じ番号が再利用されうる）。RDを`<ToRのrouter-id>:<値>`の
  ようにToR自身のグローバルに一意な識別子と組み合わせて導出するのはRFC 4364の標準的な
  やり方でありAZを跨いだ衝突は起きないが、VLAN ID単体（例:単に`65000:<vlan_id>`のような形）
  をRDとして使うと、別AZで同じ番号が再利用された別NetworkのVRFとRDが衝突しうる。RTは
  Network単位でAZを跨いで一意なため、この問題を受けない

## 3. ネットワークセグメンテーション（推奨）

以下のトラフィックは、テナントのVLAN/VRFとは**別の経路（管理系ネットワーク）**に分離することを
推奨する。

- kyuushaの各コントロールプレーンサービス↔compute-agent等のNATS通信
- compute hypervisor↔ブロックストレージノード間のNVMe-oF/TCP（またはiSCSI）通信
- Hypervisor自己登録・mTLS証明書配布のトラフィック

これらはkyuushaの制御プレーン自体の可用性に直結するため、テナントネットワークの障害
（設定ミス、輻輳等）から独立させておくことが望ましい。

## 3.5. Pure L3デプロイの場合（VLANストレッチなし）

ここまでの節（1〜3）は、host-ToR間をVLANトランクで繋ぎ、AZ内のL2ストレッチを
EVPN Type-2（MAC/IPルート）で実現する**既定の参照デプロイ**を前提にしていた
（「全体像」の直後で触れた(c): CLOS + VXLAN、AZ全体でストレッチ）。
host内のL2ドメインをVLANトランクで物理ファブリックまで延伸せず、
代わりに各VMのIPを個別にBGPで広報する構成を取りたい場合、以下の点が既定のデプロイと
異なる。この構成はL2ストレッチ自体に頼らないため、「全体像」直後で挙げた(b)
（CLOS、オーバーレイ無し、Leaf単位でL2が独立する構成）をそのまま使いたい場合の解にも
なる——host-ToR間にVLANトランクもVXLANオーバーレイも要らず、ルーテッドポートで足りる。

Pure L3デプロイはさらに、**Network間でIPアドレス空間の重複を許すかどうか**で
2通りに分かれ、VNAP参考実装（`examples/vnap-plugins/`）もそれぞれ別のスクリプトに
なる。host側のVNAPロジック（tapへのgateway_ip付与・proxy ARP・VM自身の`/32`の
FRRへの注入）は共通の設計だが、**VRFを使うかどうか**が唯一かつ決定的な分岐点になる:

- **IPアドレス空間がfabric全体で一意と保証できる場合**（NetworkClassのCIDRプールを
  ブロック内で重ならないもの——自動切り出しか、ブロック内の利用者指定——にする）: VRFは一切不要——
  全VMの`/32`をFRRの1つの共有ルーティングテーブルへ直接広報する、プレーンなBGP
  `address-family ipv4 unicast`だけで足りる。Network間の分離はkyuusha自身のIPAM
  （プールによるアドレス一意性）と、host側のNetworkInterface ACL（nftables、「2.
  VRF設計とルートリークポリシー」の「防御層（参考）」と同じ仕組み）に委ねる。
  VRFによる構造的な遮断が無いため、どちらかが崩れるとテナント間リークに直結する
  点は踏まえておくこと。**VNAPプラグインでの実現例**: `examples/vnap-plugins/
  frr-ipv4-unicast.sh`（`playground/ipv4-unicast-clos/`で実機確認済み）
- **IPアドレス空間がNetwork間で重複しうる場合**（各テナントが自分でCIDRを選ぶ
  利用者指定のCIDRプールを使う場合）: 「2. VRF設計とルートリークポリシー」と同様、
  VRFによるルーティングテーブル分離が必要で、粒度も同じNetwork単位（IP-VRF）。
  VRFの識別に要る値（L3 VNI、Route Target等）はNetworkClassのNetwork単位の参照で
  kyuushaに払い出させられ（VNAPへ`network_values`として渡る）、それをどうVRFへ
  マッピングするかはVNAPプラグイン（下記参照）とネットワークチームのFRR設定の取り決め次第——`docs/architecture.md`
  「VMのネットワーク接続をCNIのようにプラガブルにすべきか」で確定した通り、host内の
  ローカルなtap-スイッチ接続ステップだけがVNAPで差し替え可能になっている。
  **VNAPプラグインでの実現例**: `examples/vnap-plugins/frr-vrf-host-route.sh`
  （VRFへ`/32`のstatic routeを注入するところまでが共通のVNAPロジック）。
  その先、VRF間の経路をどう運ぶかは網側だけの選択で、プラグイン側の挙動は
  一切変わらない:
  - **BGP EVPN Type-5 + VXLAN**（IP-prefixルート、カプセル化あり）:
    `playground/evpn-vxlan-clos/`で実機確認済み。この検証で実際に
    `frr-vrf-host-route.sh`のバグ（tapをVRFへ`master`で所属させていなかったため、
    注入したstatic routeが常にno-opになっていた）を発見・修正した
    （詳細はスクリプト自身のコメントとdocs/release-notes.md参照）
  - **VRFスコープの素の`ipv4 unicast`eBGP**（EVPN/VXLAN無し、カプセル化無し）:
    `playground/vrf-lite-clos/`で実機確認済み。FRR 10.5.1では**unnumbered eBGPが
    非デフォルトVRFインスタンス内で確立しない**既知の制約があり、このラボは
    numbered（ポイントツーポイントアドレス方式）eBGPを使う

**VNAPプラグインの責務の境界**（両構成に共通）: どちらのプラグインも、FRRの
RIBへ（VRFの有無だけ違う）static routeを出し入れするだけで、BGP/EVPN設定自体
（ASN/RT/RD含む、host-ToR間のBGPピアリング自体も、VRFスコープかどうかも）は
本ガイドの既定デプロイと同じくネットワークチームの責務のまま。プラグイン自身は
ASN・eBGP/iBGPのどちらであるか・numbered/unnumberedのどちらであるかを一切
前提にしない・関与しない——「FRRのRIBへの出し入れ」と「その先どう運ぶか」が
きれいに分離できることが、この3スクリプト・4ラボという非対称な構成自体の
存在理由になっている。

**推奨する参照構成**（本ガイドの既定の想定であり、必須ではない。IP一意・
ipv4 unicast構成向け——VRF-lite構成は上記のFRR制約によりnumberedを使う）:
host-Leaf間は**unnumbered eBGP**（FRRの`neighbor <iface> interface remote-as
external`相当、IPv6 link-localアドレスでネイバーディスカバリするため、
リンクごとにnumberedなポイントツーポイントアドレスを用意しなくてよい）、
**ASNはハイパーバイザ1台ごとに個別に払い出す**（Leaf側は複数Leafで共通のASNでも
よい——host側だけ個体ごとに違えばeBGPのループ防止条件は満たせる）。プライベート
ASN幅は想定ハイパーバイザ台数で選ぶこと: 2-byte ASN（`64512-65534`、実質1023個）
で足りない規模なら4-byte ASN（`4200000000-4294967294`）を使う前提にしておく
（VLAN IDプールのレンジ選びと同じ「想定規模に応じて範囲を選ぶ」注意——
「1. VLANプール設計」参照）。ただしこれはあくまで参照構成の推奨であり、上記の
通りプラグイン自体はBGPセッションの設定に一切関与しないため、ネットワークチームが
iBGP+route reflector等の別構成を選んでもkyuusha側の動作に影響しない。

IP一意・ipv4 unicast構成ではさらに、ハイパーバイザ自身がSubnet外への経路を
一切持たない（VRFもVLANストレッチも無いため）——Leaf側で**host向けポートに
デフォルトルートをoriginateする**（`neighbor <host-facing iface>
default-originate`）ことが必須になる。共有NATゲートウェイ・共有DNSリゾルバ等、
local Subnet外へのあらゆるトラフィックがこの経路に頼る（`playground/
ipv4-unicast-clos/`で、実際にハイパーバイザがこの経路を受け取り機能することまで
確認済み）。

host向けToRポートは、どちらのPure L3構成でもVLANトランクである必要がない:
VMごとの`/32`ホストルートをBGPで広報するだけなので、host-ToR間のワイヤに
`vlan_id`をタグ付けする必要が無い（unnumbered/numberedいずれかのルーテッド
ポート、または単一の管理用VLANで足りる）。「1. VLANプール設計」で説明した通り、
kyuushaが払い出す`vlan_id`はこの場合もSubnetごとに排他的な番号のまま変わらないが、
それは厩舎自身のローカルな帳簿番号（Hypervisor上のブリッジ/ルーティング分離キー）
としてのみ使われ、ワイヤには一切現れない。

ゲスト側の`network-config`（`addresses`/`gateway4`)は既定デプロイと**一切変わらない**
——`internal/compute-agent/vmm/seed.go`の`buildNetworkConfig`は普通のSubnet
CIDR＋gateway4のままで良く、Pure L3固有の変更は全てVNAPプラグイン側（host内の
ローカル配線）に閉じる。

## 4. 明示的な非ゴール

以下は今回のバージョンでは対応しない。将来的に要求が出てきた場合の再検討事項として記録する。

- **RTに関係なく任意のAZ同士を無条件にメッシュ接続すること**: 必要なのは「払い出されたNetworkの
  RTについてのみ選択的にVRF間ルートを交換する」機能であり、AZ間のフルメッシュ相互接続ではない
- **マルチリージョン**: 別デプロイとして扱う。リージョンを跨ぐネットワーク設定はこのガイドの対象外

## デプロイ前チェックリスト

- [ ] AZごとにCLOSファブリック（leaf/spine）が構築され、EVPN-VXLANでAZ内のL2ストレッチが
      機能している
- [ ] AZごとのCIDRとVLAN IDの組（またはVLAN IDの範囲）がスイッチに設定され、kyuushaの
      AllocationPoolとして登録され、NetworkClassから参照されている
- [ ] host向けToRポートが、該当AZの全VLANを許可するトランクポートとして設定されている
- [ ] 1 Network = 1 VRF（AZごと）のマッピングがゲートウェイ側で設定されている
- [ ] Network間のデフォルトルートリークが**存在しない**ことを確認済み
- [ ] 共有NATゲートウェイ・共有DNSリゾルバへの経路のみが全VRFへリークされている
      （`shared_with_tenant_ids`によるNetwork共有はVRF間ルートリークを伴わないため、
      対応するチェック項目はない）
- [ ] Route Targetに使う番号範囲が整数プールとして登録され、NetworkClassのNetwork単位の参照から
      払い出されたRTが、そのNetworkの全AZのVRFインスタンスでimport/exportされている
- [ ] 管理系ネットワーク（NATS/ストレージ/Hypervisor登録）がテナントVLANと別経路になっている
