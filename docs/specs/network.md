# network仕様

## 概要

`network`サービスは`AllocationPool`・`NetworkClass`・`Network`・`Subnet`・`NetworkInterface`
を管理するCRUD+Watchサービス（設計の「なぜ」は`docs/architecture.md`「networkサービスの
リソース」参照）。NetworkとSubnetが持つ値（VLAN ID・VNI・Route Target・CIDR等）は、
Networkが参照するNetworkClassが指すAllocationPoolからkyuusha自身が払い出し、
NetworkInterfaceのIPはSubnetのCIDRから払い出す。値の意味（802.1Qタグにするか、VNIに
するか、使わないか）はkyuushaは解釈せず、VNAP/SNAPプラグインへそのまま渡す。

## リソース

```
AllocationPool（管理者）  払い出し元。組の一覧（静的）／整数・CIDR（動的）
  ▲ 参照
NetworkClass（管理者）    「どのプールから、どの単位で払い出すか」の定義
  ▲ 参照（1つ・変更不可）
Network（テナント）       ルーティングドメイン兼分離の境界。AZをまたぐ
  ├─ Subnet（テナントが依頼、kyuushaが払い出す）  AZに閉じる。CIDR 1つ＝gateway 1つ（アドレスファミリごと）
  │    └─ NetworkInterface（VMのNIC）── SecurityGroup（テナント）を明示的に付ける
  └─ 既定のSecurityGroup（Networkと一緒に作られる）
```

フィールドの詳細はproto（`proto/kyuusha/network/v1/network.proto`・`subnet.proto`・
`networkinterface.proto`・`securitygroup.proto`）参照。

- **AllocationPool**（クラスタ単位、`tenant_id`は常に空）: 管理者専用——テナントは作れず、
  見ることもできない（テナントに見えるのは自分のNetwork/Subnetに払い出された値だけ）。
  形は`oneof`で3種類（データプレーンの種類ではなく、払い出し方の形で分ける）:
  - **`entries`（組の一覧・静的）**: 管理者が登録した切り離せない組（名前付きの整数、
    アドレスファミリごとのCIDRとgateway、属性）を1つ丸ごと払い出す。典型はVLAN方式
    （ネットワーク管理者が事前にCIDRとVLAN IDを決めてスイッチに設定し、組として登録する）
  - **`integer`（整数・動的）**: 範囲（複数可）から整数を1つ。名前はNetworkClass側の参照が付ける
  - **`cidr`（CIDR・動的）**: 1つのアドレスファミリのCIDR。`USER_ANY`（利用者が自由に指定、
    重なってよい）、`USER_WITHIN_BLOCKS`（利用者が指定するが、ブロック内に収まりプール内で
    重ならないこと）、`CARVE`（ブロックから指定の長さで自動で切り出す）
  - 各組／プールはkyuushaが解釈しない属性（key/value）を持て、VNAP/SNAPに渡る
  - 払い出し済みの値が残っている間は、範囲を狭める・組や
    ブロックを消す・プールを消す、のいずれも拒否する（`FailedPrecondition`）。形
    （entries/integer/cidr）は変えられない。検証するのは書式だけで、意味は検証しない。
    プール同士の重なりも検証しない（一意であるべき範囲はどのプールを使うかで管理者が表す）
- **NetworkClass**（クラスタ単位、作成・変更・削除は管理者）: Network単位で払い出すプール
  への参照（`network`）と、zoneごとのSubnet単位の参照（`subnet`。`"*"`は全zone共通で、
  zone個別の指定は`"*"`に**足し合わせる**——同じ名前の値はzone個別が優先）。構造の検証:
  合成後の各zoneについてCIDRの出どころがアドレスファミリごとに高々1つ、同じ名前の値が
  複数のプールから来ない、Network単位の参照にCIDRプールを使わない。IPv4のCIDRの出どころが
  無いzoneにはSubnetを作れない。ほかに属性、`visibility`/`shared_with_tenant_ids`
  （誰がこのClassでNetworkを作れるか。Classには所有者がいないので`PRIVATE`は「列挙した
  テナントだけ」）、`allow_public_networks`、zoneごとの既定DNSリゾルバ、MTU、gatewayの
  決め方（先頭/末尾）、`host_aggregate_selector`（このClassのNICを持つVMを、ラベルが
  一致するcomputeの`HostAggregate`のHypervisorにだけ配置する。[VMスケジュール仕様](vm-scheduling.md)
  「HostAggregate」）を持つ。**参照でありコピーではない**: Classや
  プールの変更は以降の払い出しにだけ効く。既定のClassは持たない（Network作成時に必須）。
  Networkが参照している間は削除できない。テナントは`tenant_id`付きのGet/Listで、自分が
  使えるClassだけを見られる
- **Network**（テナント所有）: `network_class`（必須・変更不可）、`dns_suffix`、
  `visibility`/`shared_with_tenant_ids`。`status`にNetwork単位で払い出された値
  （例: `route_target`）と属性、既定のSecurityGroup（`default_security_group_id`）。
  Subnetが残っている間は削除できない
- **Subnet**: `spec`は利用者が書く依頼——`network_id`・`zone`（必須・変更不可）、
  `requested_addresses`（ClassのCIDRプールが利用者指定の場合だけ。変更不可）、
  `dns_servers`（空ならClassのそのzoneの既定）、`allocatable_ip_ranges`。`status`は
  システムが書く払い出し結果——`addresses`（実効のCIDR/gateway、アドレスファミリごと）、
  `values`（例: `vlan_id`）、`attributes`。Subnetを足せるのはNetworkの所有テナントだけ
- **NetworkInterface**: VirtualMachineとSubnetの結びつき（`VolumeAttachment`と同じ
  「結びつきそのものをリソースにする」パターン）。`spec.subnet_id`で固定するか、
  `spec.network_id`＋`spec.zone`を指定し、払い出し時にnetworkサービスがSubnetを選ぶ
  （`status.subnet_id`）。`spec.security_group_ids`は付けるSecurityGroup（下記）で、
  作成時に空ならNetworkの既定のグループが付く。作成後は`SetSecurityGroups`でだけ変えられる
  （空にすると全て拒否）
- **SecurityGroup**（テナント所有）: 下記「SecurityGroup」

## 払い出し（`internal/network/alloc.go`、`ipam.go`）

ハイパーバイザーagentは一切関与しない、network自身の中で完結する払い出し。払い出しを
行うのは単一レプリカのnetwork-reconcilerだけで、APIバイナリ（`network`）のCreateは
常に`Pending`で作るだけ——プールの帳簿はプロセス内のメモリにしか無いため（起動時に
etcd上の全Network/Subnetの`status.allocations`と全NetworkInterfaceのIP/MACから再構築する）。

- **Network**: 作成されるとClassの`network`参照から全て払い出し（全部成功するか、
  1つも払い出さない）、`Ready`になる
- **Subnet**: Networkが`Ready`になった後、Classの「そのzoneの合成済み参照」から全て
  払い出し（同じく全部か無しか）、`Ready`になる。同じNetwork内でCIDRが重なることは
  禁止（Network＝1つのルーティングドメインのため）——利用者指定のCIDRはCreate時に、
  切り出し・組のCIDRは払い出し時に、Network内の他のSubnetと重ならないものを選ぶ。
  別のNetwork同士は重なってよい。gatewayは利用者指定・組の指定がなければClassの
  `gateway_placement`（既定は先頭の利用可能アドレス）で決める
- **IPアドレス**: SubnetのIPv4 CIDRから排他的に払い出す。ネットワークアドレス・
  ブロードキャストアドレス・gatewayは対象外。`/31`・`/32`やIPv6は現状非対応で、
  常に「枯渇」として扱う（IPv6のIPAM自体は未実装。CIDR/gatewayの項目とプールは
  アドレスファミリごとに持てる形になっている）
- **Subnetの選択**: `network_id`＋`zone`で作られたNetworkInterfaceは、そのNetworkの
  そのzoneの`Ready`なSubnetを作成の古い順に試し、空きのある最初のSubnetからIPを
  払い出す——Subnetの選択とIPの払い出しが同じ処理の中で行われるので、「選んだ直後に
  枯渇した」という競合が起きない。どのSubnetにも空きがなければ`Pending`のまま
  `NoFreeAddress`のConditionで報告する（Subnetを足す合図）
- **`spec.allocatable_ip_ranges`**: 指定するとその`<開始>-<終了>`形式のIPv4範囲だけが
  払い出し対象になる。CIDRが分かっている時点（利用者指定ならCreate時、それ以外は
  Update時）に、範囲がCIDRの外に出ていないかを検証する
- **MACアドレス**: グローバルな連番から生成する簡易実装
- **管理者による修正**: システムが書く値（`status.values`/`attributes`）は、
  `SetStatusValues` RPC（Network/Subnet）で管理者だけが修正できる（ファブリック側の
  手作業の変更に合わせる等）。テナントのUpdateでは変えられない

### プール枯渇時の挙動

Quota（[Quota仕様](quota.md)参照）とは異なり、プール枯渇は**Createを拒否しない**——
リクエスト自体は正当で、他のリソースが削除されれば空きが出るかもしれないため、VMの
スケジュール失敗時と同じ考え方（[VMスケジュール仕様](vm-scheduling.md)参照）を採る。

- `Network`/`Subnet`: `Pending`のまま`Condition{type: AllocationPending, status: true}`
  を理由付きで報告する（プールの枯渇、利用者指定のCIDRがブロック外、Networkがまだ
  Readyでない等）
- `NetworkInterface`: `Pending`のまま`Condition{type: NoFreeAddress, status: true}`
  （`mac_address`は即座に払い出し済み）
- 10秒間隔の定期スイープ（`Service.Run`）が全`Pending`のNetwork/Subnet/NetworkInterfaceに
  対して再度払い出しを試みる。加えて`watchPendingNetworks`/`watchPendingSubnets`/
  `watchPendingNetworkInterfaces`が新規作成の`EventAdded`に即座に反応する——block-storageの
  `watchPendingVolumeAttachments`/`watchPendingVolumes`（[Volume仕様](volume.md)
  「排他制御」参照）と同じ形。これらのWatchは切れても最後に見た`resource_version`から
  張り直す（切れている間の変更・削除もリプレイされる）。再開点がetcdのコンパクションで
  消えていた場合は、etcd上の割当済みの値/IPをプールへ「使用中」として付け直した
  上で最初からリプレイする——このとき解放はしない（払い出し済みでまだ保存されていない
  割当を解放すると二重払い出しになりうるため）ので、その隙間で起きた削除の返却だけは
  次の再起動まで遅れる
- 成功すると同じConditionが`status: false`に更新される（削除はされない）
- 値/IPアドレスがプールへ返却されるのは、Network/Subnet/NetworkInterfaceが**実際に消えた
  とき**——network-reconcilerが自分のWatchで`EventDeleted`を観測した時点。Delete呼び出しの
  時点ではない: Finalizerが付いていればオブジェクトは`deleted_at`付きで残り、その間
  値/IPも保持され続ける。`tenant_usage`はVirtualMachineと同じ近似で最初のDelete呼び出し
  時点に1回だけ減算する。削除中（`deleted_at`付き）のNetwork/Subnetには新しい
  Subnet/NetworkInterfaceを作れない（VM Createも拒否）

## SecurityGroup

NICに付けるallowのみのルールの集まり（設計の理由は[architecture.md「SecurityGroup」](../architecture.md)）。

- **ルール**: `ingress_rules`（VMへの着信）/`egress_rules`（VMからの送信）の各要素は
  `protocol`（空=全て／`tcp`／`udp`／`icmp`）、`port_range`（宛先ポート、`tcp`/`udp`のみ、
  空=全ポート。`"22"`や`"2379-2380"`）、`peer`（相手。着信なら送信元、送信なら宛先）。
  `peer`はちょうど1つ:
  - `cidr`: IPv4/IPv6のCIDR
  - `security_group_id`: そのグループが付いているNICのアドレス全部。`"self"`はこのグループ自身
  - `network_id`: そのNetworkのNICのアドレス全部
- **評価**: NICに付いたグループのどれかのルールが許せば通る（和集合、順番に意味は無い）。
  どれにも当たらなければ着信・送信とも拒否。ステートフル（許された通信の戻りは通る）。
  グループが1つも付いていないNICは全て拒否。グループと無関係に常に通るのはゲートウェイ
  （Subnetの`gateway_ip`）との通信とARPだけで、アンチスプーフィングは別の層として常に効く
  （[SNAP仕様](snap.md)）
- **既定のグループ**: Networkが`Ready`になる前に、network-reconcilerが`default-<network id>`
  という名前のグループを所有テナントに作り、Networkの`status.default_security_group_id`に
  記録する。ルールは「着信: 同じNetworkから全て」「送信: `0.0.0.0/0`と`::/0`へ全て」で、
  変えてよい。`status.default_for_network_id`を持ち、Networkがある間は削除できない。
  Networkが消えると一緒に消える。既定のグループが無いまま`Ready`になっているNetwork
  （この機能より前に作られたもの）には、定期スイープが後から作る
- **使える人**: 所有テナント、`shared_with_tenant_ids`に入っているテナント、既定のグループ
  ならそのNetworkを使えるテナント。それ以外のテナントからは`Get`でもNotFound。付ける
  （NICの`security_group_ids`）・ルールの`peer`として参照する、のどちらにもこの条件が要る
- **検証**: 上記の形、参照するグループ／Networkが存在して所有テナントが使えること。
  NIC1つに付けられるのは16個まで
- **削除**: NICに付いている間、他のグループのルールが参照している間、既定のグループで
  Networkがある間は`FailedPrecondition`
- **アドレス集合のメンバー**: NICのアドレスが`sg:<id>`（付いているグループごと）と
  `network:<id>`の集合に入るのは、アドレスが払い出されていて、VMがどこかで`Running`の
  間（`status.hypervisor`が空でない間）だけ。VMの停止・削除と同時に抜ける

ホストでの強制と、ポリシー・アドレス集合の配り方（compute-agentがnetworkに張るxDSのような
gRPCストリーム）は[SNAP仕様](snap.md)参照。List/Watchは`tenant_id`を
空にすると全テナント分（テナント横断のロールのみ）。ラベル・アノテーション、Admission
Webhook（リソース名`SecurityGroup`）は他のリソースと同じ。

Network同士の分離をどう実現するかはデータプレーン次第: VRF系（VLAN＋ファブリックのVRF、
EVPN、VRF-lite、ホストのVRF）では経路そのものが分かれ、VRFを持たないIP一意のpure L3では
全てが1つの経路表に載るためSecurityGroupの既定拒否で分ける（SNAPの責務）。ルーティング
ドメインの値（Route Target、L3 VNI等）はNetwork単位で払い出す（Classの`network`参照）。

## Network/NetworkClassの共有と公開

- **Networkの共有**: `visibility`/`shared_with_tenant_ids`（`kyuusha.image.v1.ImageSpec`の
  同名フィールドと同じ意味）。所有テナント、`PUBLIC`なら任意のテナント、`PRIVATE`（既定）
  なら`shared_with_tenant_ids`に列挙したテナントが、このNetworkにNetworkInterfaceを
  attachできる（Networkの一部のSubnetだけを共有することはできない）。共有された
  テナントはそのNetwork、およびそのSubnetをGetで読める。判定は
  `CreateNetworkInterface`のときだけ行う
- **`PUBLIC`にできるのは`allow_public_networks`を持つClassのNetworkだけ**——所有テナントが
  相手を個別に検証しない無条件のオープン共有は、公開IPのように共有するためのアドレス空間
  に限る（[architecture.md](../architecture.md)「テナント間でのNetwork共有」参照）。
  CIDRが重ならないことはプールの構造（自動で切り出すCIDRプール、組の一覧）で管理者が
  保証し、kyuushaが改めてCIDRの一意性を検証することはしない
- **Public IP Attach**: 公開IP用のClass（`allow_public_networks`、一意なCIDRを払い出す
  プール、管理者側だけが使える`PRIVATE`のClass）で、AZごとのSubnetを束ねた公開IP用の
  Networkを管理者が作り、`PUBLIC`または`shared_with_tenant_ids`で開放する。テナントは
  そこへ2本目のNetworkInterfaceを作る——`status.ip_address`がそのまま公開IPになる
- **Classの利用許可**: `visibility`/`shared_with_tenant_ids`の判定はNetwork作成時だけ。
  既存のNetworkへのSubnetの追加では判定しないので、許可を外しても既存のNetworkは
  Subnetを足し続けられる。他テナントから共有されたNetworkにNICを付けるだけならClassの
  許可は要らない

## Create時のバリデーション

- `Network.Create`: Classが存在し呼び出しテナントが使えること、`visibility`の制約（上記）
- `Subnet.Create`: Networkが呼び出しテナント自身のもので削除中でないこと、Classの
  そのzoneにIPv4のアドレスの出どころがあること、`requested_addresses`がClassのCIDRプールの
  種類に合っていること（利用者指定なら必須、払い出し型なら指定不可）とNetwork内の他の
  Subnetと重ならないこと、`dns_servers`がIPであること
- `NetworkInterface.Create`: `subnet_id`指定ならそのSubnetが存在し`Ready`で削除中でない
  こと（`network_id`/`zone`はSubnetから決まる）。いずれの場合もNetworkが存在し、呼び出し
  テナントから使えて（上記）削除中でないこと。`network_id`＋`zone`の場合、Subnetがまだ
  無くてもよい（`Pending`で待つ）

これは「参照先が存在しない・使えない状態のリソースを作らない」という、computeのImage
検証（[Image仕様](image.md)参照）と同じ設計原則。払い出しの枯渇は上記の通りCreateを
拒否しない（Pendingで受理する）ため、この検証とは別軸。

`Subnet.Create`/`NetworkInterface.Create`はいずれも、テナントのSubnet数/NetworkInterface数
Quota（`Tenant.spec.quota.max_subnets`/`max_network_interfaces`）判定を同じCreate内で同期的に
行う（詳細は[Quota仕様](quota.md)「networkのQuota判定」参照）。カウントは常に
**呼び出し元テナント**（=実際にattachするテナント）に課金される。

## Update RPCで変えられるもの

- **Network**: `meta`と`spec`（`network_class`は変更不可）。`status`は常に保存済みの値が残る
- **NetworkClass**/**AllocationPool**: `meta`と`spec`（上記の制約付き）
- **Subnet**: `meta`と`spec`のうち`dns_servers`/`allocatable_ip_ranges`。
  `network_id`/`zone`/`requested_addresses`は変えられない（エラー）——払い出された値・
  CIDR・各NICのIP・稼働中のゲストとホストのブリッジのgatewayは全てそこから決まっている
  ため。`status`は常に保存済みの値が残る——呼び出し側が指定した`vlan_id`等を受け入れると、
  そのテナントのVMを別テナントのVLANへ配線できてしまうため
- **SecurityGroup**: `meta`と`spec`。`status`は常に保存済みの値が残る
- **NetworkInterface**: `meta`のみ。`spec.security_group_ids`は
  `SetSecurityGroups`経由でしか変えられず（差分があるとエラー）、`spec.vm_id`/
  `subnet_id`/`network_id`/`zone`の変更はエラー、`status`（`ip_address`/`mac_address`/
  `hypervisor`/`subnet_id`等）は常に保存済みの値が残る——SNAPのアンチスプーフィングは
  この`ip_address`/`mac_address`を信用するため

## `NetworkInterface.status.hypervisor`

そのNetworkInterface（IP）が今どのHypervisorにいるか。network-reconcilerが全テナントの
VirtualMachineをWatchし、VMが`Running`の間はVMの`status.hypervisor`を、それ以外
（スケジュール前、`Stopped`、`Migrating`中）は空文字列を、そのVMの全NetworkInterfaceへ
書き込む——compute-agentはBootの中でtapを配線し終えてからRunningを報告するので、
「Running＝そのHypervisorで配線済み」とみなせる。Migrateでの移動も、Migrating中は空、
移動先でRunningになった時点で移動先のHypervisorになる。Watchの取りこぼし（VMが既に
Runningの後から作ったNetworkInterface等）は、10分ごとのorphan GCスイープがVMを
Getするついでに同期し直す。

## この実装がカバーしないもの

- **クロスHypervisor接続**: tap配線自体は下記「tap配線とローカルネットワーク」の通り
  実装済みだが、同じHypervisor（同じcompute-agentプロセス、同じネットワーク名前空間）
  内で完結するタップ+ブリッジだけで、異なるHypervisorに載った同じSubnet上の2つのVM同士
  は疎通しない。物理アップリンクへの本物のVLANトランクか、VXLANのようなオーバーレイで
  ホスト間のL2を延伸する仕組みが必要で、これは別の後続マイルストーンとして未着手
  ——このスコープの絞り方自体、最初の実VM起動（[Firecracker起動仕様](firecracker-boot.md)
  参照）を「ネットワークなし」に絞った時と同じ考え方
- `NetworkInterfacePhase`の`Binding`/`Rebinding`フェーズ（compute-agentが実際の配線
  完了をnetworkへ報告する仕組み）は未実装。`status.hypervisor`は下記の通りVMの状態から
  導出している
- ネットワーク分離の物理的な実現（VRF/ルートリーク禁止によるNetwork間非疎通性、
  DNS/名前解決の拡張機能）はファブリック・VNAP側の責務。kyuushaは
  ルーティングドメインの値をNetwork単位で払い出してVNAPに渡すところまで
- **NetworkInterfaceのオーファンGC**: VM Deleteはcomputeの予約解放とcompute-agentへの
  削除コマンド送出のみ行い、そのVMが持っていたNetworkInterfaceには一切触れない
  （`compute.Reconciler.releaseIfReserved`参照）。`docs/architecture.md`が決めている
  「子リソースが親の存在を10分毎にGetで確認し、NotFoundなら自分を消す」という
  オーファンGCパターンは2026-09-13に実装済み（`network.Service.sweepOrphanedNetworkInterfaces`、
  `Service.Run`から10分間隔で起動）。この`Service.Run`（IPAM割り当て・オーファンGCを含む
  reconcileループ本体）を実際に起動するのは`network`本体（gRPC APIのみ、複数レプリカ可）
  ではなく、別バイナリ`network-reconciler`（`cmd/network-reconciler/main.go`、常に単一
  レプリカ）——`-compute-addr`もこちらが持つ。VMが存在する限り触らず、`Get`が`NotFound`
  を返した場合のみ削除する（一時的な疎通不可などその他のエラーは「わからないので消さない」
  で次回ティックに委ねる）。`network`本体（gRPC APIバイナリ）はcomputeにもNATSにも
  繋がない——ポリシーはcompute-agentの方から張るストリームで配る（[SNAP仕様](snap.md)
  「ポリシーの配布」）

## compute側の統合

VMの`spec.network_interfaces`の各要素は**Network**を指定する（`network_id`）。特定の
Subnetに固定したい場合は`subnet_id`を指定する。VMの`spec.zone`（どのAZに置くか）は
**クライアントが決める**——kyuushaがAZを選ぶことはない。Networkだけを指定した要素が
あれば`spec.zone`は必須で、Subnetを固定した要素だけならそのSubnetのzoneから決まる
（全て同じzoneでなければならない——マルチAZにまたがるVirtualMachineは作れない、という
`docs/architecture.md`の決定）。VM Create時に`compute.validateNetworkInterfaces`が
Network/Subnetの存在・利用可否・削除中でないことを検証し、決まったzoneを`spec.zone`に
保存する。スケジュールの瞬間（`Reconciler.reconcile`）にも同じ検証をやり直し、zoneを
`scheduleVM`のzoneフィルタ（[vm-scheduling.md](vm-scheduling.md)参照）に渡す。
各要素の`security_group_ids`はそのNICに付けるSecurityGroupで、空ならNetworkの既定の
グループ（上記）。computeはNICのIPが払い出された後で`NetworkInterfaceService.GetSecurityPolicy`
（api-gatewayは中継しない内部RPC）を読み、ルールとアドレス集合の写しを起動コマンドに載せる
——VMは起動した瞬間から正しいルールで動く。

実際のNetworkInterfaceオブジェクトの作成は、VMがPhaseScheduledになった時点で行う
（まだ一度もスケジュールされていないVMのために作るとオーファンになるため）。`name`は
`iface-<vm-id>-<index>`という決定的な命名（`docs/architecture.md`「子リソースのID命名規則」
参照）で、これがCreateの冪等性キーになる。Networkだけの要素は`network_id`＋`zone`で
作り、networkサービスがSubnetを選ぶ（上記「払い出し」）。作成に成功した
NetworkInterfaceのIDは`VirtualMachineStatus.interface_refs`に書き込まれる。

compute-agentへは、払い出されたIP/MACに加えて、そのSubnetのCIDR/gateway、Network/
NetworkClassの文脈（`network_id`、Networkのラベル・払い出された値・属性、Classの名前・
属性・MTU、Subnetの払い出された値・属性）を渡す——compute-agentはnetworkサービスの
クライアントを持たないため（Imageのkernel/rootfs解決と同じ理由）。computeは
`NetworkService`/`NetworkClassService`のクライアントも持つ（`cmd/compute`・
`cmd/compute-reconciler`が設定）。

`network_interfaces`は必須ではない（空配列でも良い）。空の場合、zoneは強制されず、
NetworkInterfaceも作られない。

CLIからは`kyuusha vm create -networks=<id1>,... -zone=<zone>`（Subnetを固定するなら
`-subnets=<id>,...`）で指定でき、先頭が`primary`になる。

## tap配線とローカルネットワーク

「compute-agentに統合する」という上記の設計方針を実装したのが
`internal/compute-agent/netsetup`。`Reconciler.reconcile`のPhaseScheduledケースで
作られた各NetworkInterface（IP/MAC）とそのSubnet（CIDR/gateway）は
`compute.CreateCommand.interfaces`としてcompute-agentに渡り、compute-agentがそこから
実際のtapデバイスを作る。

組み込みの配線（`-network-attach-bin`未指定）では、Subnetごとに1つのLinuxブリッジ
（`kbr`＋Subnet IDから導出した12桁の16進）をcompute-agent自身のネットワーク名前空間内に
作り、そのブリッジにSubnetのgatewayをそのまま割り当てる——ブリッジ自身が「そのSubnetの
ローカルなゲートウェイ」として機能する。Subnetに払い出された値（VLAN ID等）はホストの外へ
出ない組み込みの配線には意味が無いので使わない（それを使うのは`vlan-trunk.sh`等の
VNAPプラグイン）。NetworkInterfaceごとに専用のtapデバイス（名前は長いIDから決定的に導出
した短い名前。Linuxのインタフェース名は15文字まで）を作ってそのブリッジにmasterとして
繋ぎ、Firecrackerの`network-interfaces`設定（`host_dev_name`/`guest_mac`）にそのtapを渡す。

ゲストIPの設定にはDHCPもkernelのIP autoconfigurationも使っていない——確実に効く保証が
ないため。代わりに、compute-agentがboot_argsへ`kyuusha.net.<index>.ip=<ip>/<prefix>`
のような独自のカーネルコマンドライン規約を追記し、ゲスト側の`/init`
（`docker/fc-guest-init.sh`）が`/proc/cmdline`から直接パースして`ip addr add`
する。`primary`なインタフェースだけがゲスト側のデフォルトルートを持つ。規約の一覧:
`kyuusha.net.<i>.ip=<ip>/<prefix>`、`.gw=<gateway>`、`.primary=1`、`.mtu=<mtu>`（NetworkClassの
`mtu`、0なら付けない）、`.dns=<ip>[,<ip>...]`と`.search=<domain>`（primaryのみ。
リゾルバはSubnetの`dns_servers`、空ならNetworkClassのそのzoneの既定（無ければ`"*"`の既定）、
searchはNetworkの`dns_suffix`——`/init`が`/etc/resolv.conf`に書く）。cloud-init NoCloudの
network-config（[cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)参照）にも同じ
MTU（`mtu`）とリゾルバ（`nameservers`）を書く。設定が終わると
`/init`はそのインタフェースのgateway_ip（＝ホスト側ブリッジのIP）へpingを打ち、結果を
シリアルコンソールに書く（`kyuusha vm console`で確認できる）——tap配線が実際に機能して
いることを、2台目のVMを用意しなくても1台のコンソール出力だけで確認できるようにする
ための自己診断。さらに`/init`は2秒ごとにeth0の/24の先頭の数十アドレスへpingを打ち、到達性が変わった
アドレスだけを`kyuusha: neighbor <ip> reachable|unreachable`としてコンソールに書く——
VM同士の通信をSecurityGroupが許しているか、ゲストの中からしか見えない挙動をplaygroundで
確かめるための診断。

**`user_data`注入(cloud-init NoCloud seed disk)とは別物**: `spec.user_data`全体を
ゲストへ注入する仕組みは実装済み（[Firecracker起動仕様](firecracker-boot.md)
「UserData注入」参照、`internal/compute-agent/fcvmm/seed.go`）だが、ここで説明した
tap配線のIP設定用カーネルコマンドライン規約とは独立した別経路——うちの最小限自作
Alpine rootfsにはcloud-init自体が入っておらず、`/init`（`docker/fc-guest-init.sh`）は
seed diskの中身をコンソールへ書き出すだけで実際には解釈しない。tap配線のIP設定は
今後もこのカーネルコマンドライン規約のまま独立させる想定で、両者を統合する予定はない。

tapデバイスの生成にはbusybox `ip`にない`tuntap add`ではなく、`/dev/net/tun`への
`TUNSETIFF`/`TUNSETPERSIST` ioctl（`golang.org/x/sys/unix`）を直接使っている——
compute-agentコンテナ・ゲストrootfsのどちらもbusybox `ip`しか持たないため。VM Stop時
（`fcvmm.Manager.Stop`→プロセスが実際に終了した後の後始末）にtapは削除されるが、
ブリッジ自体は残す（同じHypervisor上の他のVMと共有されるため）。

この機能には`/dev/net/tun`と`CAP_NET_ADMIN`が要る。`/dev/kvm`と同様、なければこの
機能だけが動かず（VMはError相当になる）、それ以外のスタックには影響しない。

## VNAP・SNAP（プラガブルなtap配線・ACL強制プラグイン契約）

tapのローカルスイッチへの配線は[VNAP仕様](vnap.md)、SecurityGroupの
ホスト側での強制は[SNAP仕様](snap.md)を参照——両者は同じ「バイナリ+stdin JSON+
exit code」という呼び出し規約を共有するが、配線とACL強制は直交する別々の関心事のため
独立したプラグイン契約になっている（それぞれ`-network-attach-bin`/
`-security-backend-bin`）。

## エンドポイント

`network :8084`（`AllocationPoolService`, `NetworkClassService`, `NetworkService`,
`SubnetService`, `NetworkInterfaceService`, `SecurityGroupService`、それにcompute-agent向けの
`kyuusha.network.agent.v1.PolicyDistributionService`）。api-gateway経由でのみ到達可能
（[システム構成仕様](system-overview.md)参照）——`SubnetService`/
`NetworkInterfaceService`は組み込みのプロキシで、残りはapi-gatewayの外部バックエンドと
同じ汎用転送（[外部システム連携仕様](external-integration.md)「外部バックエンドの登録」）で
`kyuusha.network.v1.*`ごと転送する。AllocationPool・NetworkClassの作成/変更/削除は
`tenant_id`を持たないので、テナント横断のロール（`admin`、`network-admin`等）だけが通る。CLIは[CLI仕様](cli.md)参照。
