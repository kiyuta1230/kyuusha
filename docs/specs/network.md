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
  再トリガーしないため、VMスケジュールの定期スイープと同じ理由で必要）。加えて
  `watchPendingSubnets`/`watchPendingNetworkInterfaces`（`Service.Run`から起動する
  別goroutine）が新規作成された`Pending`のSubnet/NetworkInterfaceの`EventAdded`に即座に
  反応し、10秒の定期スイープを待たず払い出しを試みる——block-storageの
  `watchPendingVolumeAttachments`/`watchPendingVolumes`（[Volume仕様](volume.md)
  「排他制御」参照）と同じ形
- 成功すると同じConditionが`status: false`に更新される（削除はされない）
- SubnetのDelete/NetworkInterfaceのDeleteは、`Ready`で実際に払い出し済みだった場合のみ
  VLAN ID/IPアドレスをプールへ返却する

## `spec.mesh_group`（宣言のみ、自動許可はまだ）

同一テナントが複数のAZにまたがってSubnetを持つ場合（AZ毎に別Subnet/別VLANになる設計、
`docs/architecture.md`「マルチAZにまたがるVirtualMachineは作れない」参照）、AZ間の
**経路**はRoute Targetによる自動交換で疎通するが、`NetworkInterfaceSpec.ingress_rules`/
`egress_rules`の**ACL**はSubnet CIDR外を既定で拒否するため（下記「セキュリティ
バックエンド」参照、これ自体は実装済み）、AZ間で通信したい場合は本来Subnetの組み合わせ
ごとに手動でallowルールを書く必要がある。

`spec.mesh_group`は、この手間を減らすための**意図の宣言**フィールド（`shared_with_tenant_ids`
と同じ位置づけ）: 同一テナント内で同じ`mesh_group`値を持つSubnet同士は、デフォルトで
互いを許可する対象とみなす、という設計上の意図だけを表す。**現状これを実際に自動許可へ
反映する経路はまだ無い**（`shared_with_tenant_ids`によるクロステナントCIDR許可の検証も
同様に未実装）——ACL強制自体（`ingress_rules`/`egress_rules`）は実装済みだが、
`mesh_group`一致を根拠に暗黙のallowルールを注入する処理はまだ`UpdateFirewallRules`にも
Create時にも組み込まれておらず、[open-questions.md](../open-questions.md)の未解決事項
として残っている。

## Create時のバリデーション

`NetworkInterface.Create`は`spec.subnet_id`が指す`Subnet`が存在し、同じテナントに属し、
`Ready`であることを検証する（存在しない/他テナント/未Readyなら`ErrValidation`）。これは
「参照先が存在しない・使えない状態のリソースを作らない」という、computeのImage検証
（[Image仕様](image.md)参照）と同じ設計原則。IPAM自体のプール枯渇は上記の通りCreateを
拒否しない（Pendingで受理する）ため、この検証とは別軸。

`Subnet.Create`/`NetworkInterface.Create`はいずれも、上記の検証に加えてテナントの
Subnet数/NetworkInterface数Quota（`Tenant.spec.quota.max_subnets`/
`max_network_interfaces`）判定を同じCreate内で同期的に行う。`NetworkInterface`側は
このQuota判定を上記のSubnet存在/Ready検証より後に行う（詳細は
[Quota仕様](quota.md)「networkのQuota判定」参照）。

## この実装がカバーしないもの

- **クロスHypervisor接続**: tap配線自体は下記「tap配線とローカルネットワーク」の通り
  実装済みだが、同じHypervisor（同じcompute-agentプロセス、同じネットワーク名前空間）
  内で完結するタップ+ブリッジだけで、異なるHypervisorに載った同じSubnet上の2つのVM同士
  は疎通しない。物理アップリンクへの本物のVLANトランクか、VXLANのようなオーバーレイで
  ホスト間のL2を延伸する仕組みが必要で、これは別の後続マイルストーンとして未着手
  ——このスコープの絞り方自体、最初の実VM起動（[Firecracker起動仕様](firecracker-boot.md)
  参照）を「ネットワークなし」に絞った時と同じ考え方
- `NetworkInterface.status.hypervisor`は現状常に空文字列のまま（tap配線が実装された今も
  未実装）。networkサービス側でどのHypervisorに実際にバインドされたかを追跡するには、
  `NetworkInterfacePhase`にすでに用意されている`Binding`/`Rebinding`フェーズを使った
  compute-agent→network側への報告の仕組みが要るが、tap配線そのものとは別の作業として
  切り出している
- ネットワーク分離の実現方式（VRF/ルートリーク禁止によるテナント間非疎通性、
  DNS/名前解決の拡張機能）は設計のみ（`docs/architecture.md`参照）、実装はまだ
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
  で次回ティックに委ねる）。`network`本体（gRPC APIバイナリ）も別の理由で同じ
  `computeClient`を独自に持つ——`UpdateFirewallRules`がACL変更を通知すべきHypervisorを
  解決するためで、こちらはオーファンGCとは無関係（下記「セキュリティバックエンド」参照）

## compute側の統合

VM Create時、`spec.network_interfaces`の各要素（`subnet_id`）についてSubnetの存在/
同一テナント/`Ready`を検証し（`compute.validateNetworkInterfaces`、上記「Create時の
バリデーション」と同じ形）、参照先Subnetがすべて同じzoneであることも合わせて検証する
（マルチAZにまたがるVirtualMachineは作れない、という`docs/architecture.md`の決定を
ここで強制する）。この検証で得たzoneは、VMがPhasePendingからスケジュールされる
瞬間（`Reconciler.reconcile`）にもう一度Subnetを引き直して再計算し、
`scheduleVM`のzoneフィルタ（[vm-scheduling.md](vm-scheduling.md)参照）に渡す
——Create時点とスケジュール時点の間でSubnetが変わる可能性があるため、キャッシュせず
毎回引き直す。

実際のNetworkInterfaceオブジェクトの作成は、VMがPhaseScheduledになった時点
（`Reconciler.reconcile`のPhaseScheduledケース）で行う。まだ一度もスケジュールされて
いないPending中のVMのために先にNetworkInterfaceを作ってしまうと、そのVMが結局
一度も動かないまま終わった場合にオーファンになるため。作成する各NetworkInterfaceの
`name`は`iface-<vm-id>-<index>`という決定的な命名（`docs/architecture.md`
「子リソースのID命名規則」参照）で、これがそのままCreateの冪等性キーになる
——reconcileが同じVMに対して複数回呼ばれても（例えば直前のUpdateが失敗して
リトライされても）二重に作られることはない。作成に成功したNetworkInterfaceの
IDは`VirtualMachineStatus.interface_refs`に書き込まれ、`Phase = Provisioning`
への遷移と同じUpdateで永続化される。

`network_interfaces`は`image_id`と違い必須ではない（空配列でも良い）。空の場合、
zoneは強制されず（`scheduleVM`のzoneフィルタは無効化）、NetworkInterfaceも
作られない——ネットワークなし・シリアルのみのVMという、現状のFirecracker実VM起動の
第一段階（[firecracker-boot.md](firecracker-boot.md)参照）とも整合する。

CLIからは`kyuusha vm create -subnets=<id1>,<id2>,...`で指定でき、先頭のSubnetが
`primary`になる。

## tap配線とローカルネットワーク

「compute-agentに統合する」という上記の設計方針を実装したのが
`internal/compute-agent/netsetup`。`Reconciler.reconcile`のPhaseScheduledケースで
作られた各NetworkInterface（IP/MAC）とそのSubnet（CIDR/gateway_ip/vlan_id）は
`compute.CreateCommand.interfaces`としてcompute-agentに渡り（compute-agentはnetwork
サービスのクライアントを一切持たない——Imageのkernel/rootfs解決と同じ理由）、
compute-agentがそこから実際のtapデバイスを作る。

具体的には、`vlan_id`ごとに1つのLinuxブリッジ（`kbr<vlan_id>`）をcompute-agent自身の
ネットワーク名前空間内に作り、そのブリッジにSubnetの`gateway_ip`をそのまま割り当てる
——ブリッジ自身が「そのSubnetのローカルなゲートウェイ」として実際に機能するようにして
いる。NetworkInterfaceごとに専用のtapデバイス（名前は`netif-...`のような長いIDから
決定的に導出した短い名前。Linuxのインタフェース名は15文字までしか使えないため）を
作ってそのブリッジにmasterとして繋ぎ、Firecrackerの`network-interfaces`設定
（`host_dev_name`/`guest_mac`）にそのtapを渡す。

ゲストIPの設定にはDHCPもkernelのIP autoconfigurationも使っていない——確実に効く保証が
ないため。代わりに、compute-agentがboot_argsへ`kyuusha.net.<index>.ip=<ip>/<prefix>`
のような独自のカーネルコマンドライン規約を追記し、ゲスト側の`/init`
（`docker/fc-guest-init.sh`）が`/proc/cmdline`から直接パースして`ip addr add`
する。`primary`なインタフェースだけがゲスト側のデフォルトルートを持つ。設定が終わると
`/init`はそのインタフェースのgateway_ip（＝ホスト側ブリッジのIP）へpingを打ち、結果を
シリアルコンソールに書く（`kyuusha vm console`で確認できる）——tap配線が実際に機能して
いることを、2台目のVMを用意しなくても1台のコンソール出力だけで確認できるようにする
ための自己診断。

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

## VNAP（ローカルなtap配線プラグイン契約）

`docs/architecture.md`「VMのネットワーク接続をCNIのようにプラガブルにすべきか」
（解決済みリスト参照）で確定した設計の実装。`-network-attach-bin`（compute-agentの
フラグ）を指定すると、上記「tap配線とローカルネットワーク」の**スイッチへの実配線
ステップだけ**を外部バイナリへ委譲できる——tapデバイス自体の作成・削除は常に
`netsetup`（厩舎自身）が担う。未指定（既定）なら今まで通り固定のLinuxブリッジ実装
のまま、既存の挙動は一切変わらない。

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

**参考実装**: `examples/vnap-plugins/frr-type5.sh`——EVPN Type-5（pure L3）
デプロイ向けのサンプル（[network-deployment-guide.md](../network-deployment-guide.md)
「3.5. Type-5（EVPN pure L3）デプロイの場合」参照）。共有ブリッジを使わず、
VMごとのtapへ`gateway_ip`を`/32`で直接付与しproxy ARPを有効化した上で、VM自身の
IPを`/32`のホストルートとしてカーネルとFRR（`vtysh`経由）の両方へ注入する。
playgroundで実機確認済み（Firecrackerゲストがブリッジ無しで実際に起動しゲスト
自身がgatewayへのpingに成功、VM削除時にFRR側のルートも正しく引き上げられることを
確認）。

## セキュリティバックエンド（ホスト側ACL強制プラグイン契約）

`ingress_rules`（VMへの着信を許可するルール）/`egress_rules`（VM発の送信を許可する
ルール）は、VNAPと同じ設計思想でプラガブルにしたホスト側ACL強制エンジンによって実際に
強制される。`-security-backend-bin`（compute-agentのフラグ）を指定すると、tapの
ワイヤリング（VNAP）とは独立に外部バイナリへ委譲できる——未指定（既定）なら
`internal/compute-agent/nftacl`（後述）が担う。VNAPと1つのプラグインに統合していない
理由: 配線（tapをどのスイッチに繋ぐか）とACL強制（何を通すか）は直交する関心事で、
片方だけ差し替えたい運用（例: 既定のLinuxブリッジ配線のままeBPF/OVS ACLだけ独自実装
に差し替える）に対応するため。

- **契約はVNAPと同型**（`<bin> attach`/`<bin> detach`をexec、標準入力にJSON、成否は
  exit codeのみ、タイムアウト10秒、プラグイン側が冪等性の責務を負う）——ただし
  ペイロードの中身もフラグも別（`-network-attach-bin`とは無関係）
- **attachのpayload**: `tap_name`/`iface_id`/`vm_id`/`tenant_id`/`subnet_cidr`/
  `gateway_ip`/`ingress_rules`/`egress_rules`
- **呼び出しタイミング**: VM Boot時（`netsetup.Wire`成功直後）と、`UpdateFirewallRules`
  呼び出し後の再適用時（後述、NATS経由）の両方
- **detachのpayload**: `tap_name`/`iface_id`/`vm_id`/`tenant_id`のみ（VNAPのdetachと
  同じ理由）
- **失敗時の扱い**: Boot時のattach失敗はVNAPのattach失敗と同じエラー経路（Boot全体が
  失敗、`cleanup()`が後始末）。detach失敗はログのみで継続

### デフォルト実装: `internal/compute-agent/nftacl`（bridgeファミリ）

`nft`コマンドをexecする実装（Go nftablesライブラリへの依存は追加しない、`netsetup`の
`ip`exec方式と同じ流儀）。**netdevファミリ（tapごとの独立したingress/egressフック）
ではなく、bridgeファミリのforwardフックを使う**——実機検証の結果、netdevファミリでは
`ct state`（確立済み接続の自動許可）が使えないことが判明したため（"Protocol error"。
netdevフックはconntrackが確立されるより前段のフックで、環境依存の問題ではなくnetdev
ファミリ自体の制約）。この設計上の代償として、**このデフォルト実装はtapがLinuxブリッジ
のポートであることを前提とする**——VNAPで非ブリッジ配線（`examples/vnap-plugins/
frr-type5.sh`のようなEVPN Type-5 pure L3構成）を使う場合、`-security-backend-bin`で
別の（ブリッジを前提としない）セキュリティバックエンドを組み合わせる必要がある。

- 1つの共有base chain（`bridge kyuusha_acl base`、`hook forward`、`policy accept`——
  このフックはホスト上の全ブリッジ転送トラフィックに発火するため、kyuushaが管理しない
  トラフィックに影響してはならない）が、稼働中のtapごとに`iifname "<tap>" jump <tap>-in`
  /`oifname "<tap>" jump <tap>-out`という2本のガード規則を持つ
- `<tap>-in`（`iifname`一致＝VM自身が送信した通信）が`egress_rules`を、`<tap>-out`
  （`oifname`一致＝VMへ配送される通信）が`ingress_rules`を強制する——tap自身を主語にした
  netfilterの方向と、VMを主語にしたspecの命名は逆になる点に注意
- 各チェーン内は`accept`ではなく`return`で「許可」を表す（VM間通信は1回のforwardフックで
  送信元・宛先両方のtapのチェーンを通過する必要があるため、`accept`で早期終了すると
  もう一方のチェーンが評価されなくなってしまう）。`drop`で明示的な拒否、チェーン末尾にも
  `drop`（該当ルール無しはデフォルト拒否）。両チェーンをreturnで通過した後、実際に許可
  するのはbase chain自身の`policy accept`
- 実装済み・playground実機確認済み（VM起動直後のデフォルト拒否ベースライン、
  `UpdateFirewallRules`後のルール反映を確認——詳細はdocs/release-notes.md参照）。
  「compute-agentプロセスの再起動を跨いでnftablesルールが残る」という主張自体は
  正しい（`ip netns`を破棄しない限りnftablesはカーネル側の状態でありプロセスの
  生死に依らない、tapの`TUNSETPERSIST`と全く同じ理屈）が、**playground環境では
  実機確認できない**——`docker compose restart compute-agent-N`はコンテナの
  ネットワーク名前空間ごと作り直すため、tap/nftablesを含むそのnetns内の状態が
  丸ごと失われ、かつコンテナ内の全プロセス（Firecracker/jailerの子プロセスも
  含む）が道連れに終了する。これはベアメタル環境でcompute-agentだけを
  （systemd等で）再起動する場合とは異なるDocker特有の挙動で、今回のACL機能に
  限らずVNAP以来のtap永続化の前提そのものに付随する、コンテナ化playground側の
  制約として認識しておく

### 非ブリッジ配線向けの参考実装: `examples/security-plugins/ebpf-secacl`

`nftacl`はtapがLinuxブリッジのポートであることを前提とするため、VNAPで非ブリッジ
配線（`examples/vnap-plugins/frr-type5.sh`のようなEVPN Type-5構成）を使う場合は
`-security-backend-bin`で別のセキュリティバックエンドを組み合わせる必要がある、と
上で述べた。その具体例として、TC-BPF（tapデバイスのclsact ingress/egress両フックに
`cilium/ebpf`で直接アタッチ、ブリッジのポートである必要が無い）によるステートフルな
参考実装を`examples/security-plugins/ebpf-secacl/`に用意した——VNAPの
`examples/vnap-plugins/`と同じ「アダプトして使う参考実装」という位置づけ（本体の
compute-agentイメージ・ビルドには組み込まない、独立したGoモジュール）。

netfilterのconntrackが使えないTC-BPFフック向けに、自前の正規化5-tupleベースの
conntrack相当（BPFの`LRU_HASH`マップ、全tap共有）を実装しており、`nftacl`の
`ct state established,related`と同等の双方向ステートフル動作を実機（veth
ペア+network namespaceでの実トラフィック）で確認済み。詳細・設計判断・実機確認結果は
`examples/security-plugins/ebpf-secacl/README.md`参照。性能重視のステートレス版は
別途後日の課題。

### `UpdateFirewallRules`とホストへの反映

`NetworkInterfaceService.UpdateFirewallRules`（`ingress_rules`/`egress_rules`を
まとめて置き換える専用RPC、`resource_version`は持たない——Resize等と同じ
Get-then-mutate-then-Update規約）は、etcdへの書き込みに成功すると、対象VMが現在
稼働しているHypervisorへNATS経由（`ms.network.cmd.<hypervisor>.network_interface.
update_acl`、`network`独自のJetStreamストリーム`NETWORK_CMD`）でベストエフォート通知
する（`internal/network/service.go`の`publishUpdateACL`）。Hypervisor解決に失敗する・
まだスケジュールされていない・NATS publish自体が失敗、のいずれも通知を諦めるだけで
RPC自体は成功のまま返す（etcdへの反映は既に完了しているため）。

compute-agent側は`update_acl`コマンドを受信すると、稼働中の全ドライバへ
`ApplyACL(vm_id, iface_id, ...)`を試し、該当tapを持つドライバが見つかるまでメッセージを
**Ackしない**——JetStreamの再配送（デフォルトAckWait、`MaxDeliver=20`で打ち切り）に
そのまま「VM起動待ち」のリトライを任せる。`UpdateACLCommand`は常にその時点の
**全ルールセット**を運ぶ（差分ではない）ため、再配送や順序前後があっても最終的に
正しい状態へ収束する。ただし`resource_version`を使い、より新しいコマンドが既に適用済み
なら古いコマンドを再適用しない（compute-agent再起動でこの記録が失われても問題ない——
次に来る`update_acl`が常に完全な状態を運ぶため）。

## エンドポイント

`network :8084`（`SubnetService`, `NetworkInterfaceService`）。api-gateway経由でのみ
到達可能（[システム構成仕様](system-overview.md)参照）。CLIは[CLI仕様](cli.md)参照。
