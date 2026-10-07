# SNAP（Security Network Attach Protocol）仕様

NICに付いたSecurityGroup（[network仕様](network.md)「SecurityGroup」）は、VNAP（[VNAP仕様](vnap.md)参照）と同じ設計思想でプラガブルにしたホスト側
ACL強制プラグイン契約——SNAP（Security Network Attach Protocol）——によって実際に
強制される。`-security-backend-bin`（compute-agentのフラグ）を指定すると、tapの
ワイヤリング（VNAP）とは独立に外部バイナリへ委譲できる——未指定（既定）なら
`internal/compute-agent/nftacl`（後述）が担う。VNAPと1つのプラグインに統合していない
理由: 配線（tapをどのスイッチに繋ぐか）とACL強制（何を通すか）は直交する関心事で、
片方だけ差し替えたい運用（例: 既定のLinuxブリッジ配線のままeBPF/OVS ACLだけ独自実装
に差し替える）に対応するため。

- **契約はVNAPと同型**（`<bin> attach`/`<bin> detach`/`<bin> update_sets`をexec、
  標準入力にJSON、成否はexit codeのみ、タイムアウト10秒、プラグイン側が冪等性の責務を
  負う）——ただしペイロードの中身もフラグも別（`-network-attach-bin`とは無関係）
- **ルールとアドレス集合**: ルールはそのNICのSecurityGroupのルールを1つにまとめた
  allowのみの一覧で、相手がSecurityGroupやNetworkのものは**アドレスに展開せず、名前付きの
  アドレス集合への参照のまま**渡す（`sg:<id>`、`network:<id>`）。集合の中身はルールと
  別に配り、メンバーの増減は差分で届く（後述「アドレス集合の配布」）。どのルールにも
  当たらない通信は両方向とも拒否する。グループと無関係に常に通すもの（ホストの基本動作）は
  確立済みの通信の戻り、ARP、`gateway_ip`との通信だけ
- **attachのpayload**: `tap_name`/`iface_id`/`vm_id`/`tenant_id`/`subnet_id`/
  `subnet_labels`/`subnet_cidr`/`gateway_ip`/`ip_address`/`mac_address`、
  `security_group_ids`、`ingress_rules`/`egress_rules`（各要素は`protocol`（空=全て／
  `tcp`／`udp`／`icmp`）、`port_range`（宛先ポート、空=全ポート）、`cidr`か`set`の
  どちらか）、`sets`（ルールが参照する集合の**全量**: `name`/`version`/`members`）、
  およびVNAPと同じNetwork/NetworkClassの文脈（`network_id`/`network_class`/
  `subnet_values`等、[VNAP仕様](vnap.md)参照）。`subnet_labels`はSubnetの`meta.labels`、
  `ip_address`/`mac_address`はそのVMに払い出された自身のアドレスでアンチスプーフィング
  （後述）の入力——再適用でも毎回同じ値が届く（networkサービスがポリシーの配布に
  載せる）。ルールが参照しているのに`sets`に無い集合は、既にホストにある中身のまま使う
  （無ければ空として作る）。プラグインは知らないフィールドを無視すること
- **update_setsのpayload**: `{"sets": [...]}`。各要素は`name`/`version`と、`full: true`
  なら`members`（全量で置き換え）、そうでなければ`add`/`remove`（差分）。このホストの
  どのNICのルールも参照していない集合はプラグインが無視してよい
- **呼び出しタイミング**: attachはVM Boot時（`netsetup.Wire`成功直後）と、ポリシーの
  再適用時（ポリシーの配布、後述）。update_setsはアドレス集合の変更が届いた時
- **版**: 集合の`version`はetcdのリビジョンで、差分と全量の新旧を同じ物差しで比べられる。
  compute-agent（`internal/compute-agent/snap`）が集合ごとに最後に適用した版を覚えて
  おり、差分はそれより新しい場合だけ、全量はそれ以上の場合だけプラグインへ渡す（attachの
  `sets`も同じで、ホストが既に持つより古い写しは渡さない）。プラグインは版を見なくてよい
- **detachのpayload**: `tap_name`/`iface_id`/`vm_id`/`tenant_id`のみ（VNAPのdetachと
  同じ理由）
- **失敗時の扱い**: Boot時のattach失敗はVNAPのattach失敗と同じエラー経路（Boot全体が
  失敗、`cleanup()`が後始末）。detach失敗はログのみで継続

## デフォルト実装: `internal/compute-agent/nftacl`（bridgeファミリ）

`nft`コマンドをexecする実装（Go nftablesライブラリへの依存は追加しない、`netsetup`の
`ip`exec方式と同じ流儀）。**netdevファミリ（tapごとの独立したingress/egressフック）
ではなく、bridgeファミリのforwardフックを使う**——実機検証の結果、netdevファミリでは
`ct state`（確立済み接続の自動許可）が使えないことが判明したため（"Protocol error"。
netdevフックはconntrackが確立されるより前段のフックで、環境依存の問題ではなくnetdev
ファミリ自体の制約）。この設計上の代償として、**このデフォルト実装はtapがLinuxブリッジ
のポートであることを前提とする**——VNAPで非ブリッジ配線（`examples/vnap-plugins/
frr-vrf-host-route.sh`/`frr-ipv4-unicast.sh`のようなpure L3構成）を使う場合、
`-security-backend-bin`で
別の（ブリッジを前提としない）SNAP実装を組み合わせる必要がある。

- 1つの共有base chain（`bridge kyuusha_acl base`、`hook forward`、`policy accept`——
  このフックはホスト上の全ブリッジ転送トラフィックに発火するため、kyuushaが管理しない
  トラフィックに影響してはならない）が、稼働中のtapごとに`iifname "<tap>" jump <tap>-in`
  /`oifname "<tap>" jump <tap>-out`という2本のガード規則を持つ
- `<tap>-in`（`iifname`一致＝VM自身が送信した通信）が`egress_rules`を、`<tap>-out`
  （`oifname`一致＝VMへ配送される通信）が`ingress_rules`を強制する——tap自身を主語にした
  netfilterの方向と、VMを主語にしたspecの命名は逆になる点に注意
- 各チェーン内は`accept`ではなく`return`で「許可」を表す（VM間通信は1回のforwardフックで
  送信元・宛先両方のtapのチェーンを通過する必要があるため、`accept`で早期終了すると
  もう一方のチェーンが評価されなくなってしまう）。チェーン末尾は`drop`（どのルールにも
  当たらなければ拒否）。両チェーンをreturnで通過した後、実際に許可するのはbase chain自身の
  `policy accept`
- ルールも常時許可（`gateway_ip`）も**相手側**のアドレスで判定する——`<tap>-in`
  （VMの送信）は宛先、`<tap>-out`（VMへの着信）は送信元。VM自身の側で判定すると、
  アンチスプーフィングで送信元が自分のIPに固定されている以上常に一致し、ルールが
  一切効かなくなる
- **アドレス集合はnftablesの名前付きset**（`type ipv4_addr`、名前は集合名のハッシュ
  `ks_<12桁>`）で、bridge・inetの両テーブルに1つずつ置き、その集合を参照する全tapの
  ルール（`ip daddr @ks_...`/`ip saddr @ks_...`）が共有する。update_setsは該当setの中身を
  書き換えるだけでtapのチェーンには触らない（差分は現在の中身を読んで作り直し、setごとに
  1つのnftトランザクションで適用する）。どのtapからも参照されなくなったsetはtapの
  detach時に消す（参照中のsetはnftが削除を拒むので、それを判定に使う）。IPv6のCIDRの
  ルールは`ip6`で書く（アンチスプーフィングがVMからのIPv6を落とすので、実際に効くのは
  着信側だけ）
- **ホストでルーティングされる通信にも同じルールを効かせる**: bridgeのforwardフックが
  見るのは同じブリッジ内で転送されるフレームだけで、VMがSubnetの外へ出る通信（ゲートウェイ
  ＝ブリッジ自身宛てにローカル配送される）や、`ip_forward`が有効なホストでのブリッジ間の
  ルーティング（Dockerは`ip_forward`を有効にする）は通らない。そのため同じtapごとの
  チェーンを`inet kyuusha_acl`テーブルにも書き、`forward`/`input`/`output`の各フックから
  （ルーティング後はtap名が見えないので）「ブリッジ名＋VMのIP」で呼ぶ——VMのIPは
  アンチスプーフィングで保証済み。これが無いと、ルールの無いVM同士でもホスト経由で
  別テナントのSubnetへ到達できてしまう
- `<tap>-in`/`<tap>-out`はどちらもARP（`ether type arp`）を常に通す——ARPはIPv4では
  ないので`ip saddr`/`ip daddr`のベースラインに一致せず、conntrackも追跡しないため、
  通さないと同じブリッジ上のVM同士がそもそも互いのMACを解決できない。ARPの正当性の
  検査は下記のアンチスプーフィングが担う

### アンチスプーフィング

SecurityGroupとは独立に、VMが送信する全フレームについて次を強制する
（テナントがSecurityGroupで緩めることはできない）:

- Ethernetの送信元MAC = 払い出された`mac_address`
- IPv4なら送信元IP = 払い出された`ip_address`
- ARPなら送信者MAC = `mac_address`、送信者IP = `ip_address`または`0.0.0.0`
  （RFC 5227のARP probe）
- それ以外のEtherType（IPv6、802.1Qタグ付きフレーム等）はdrop——kyuushaはIPv6
  アドレスを払い出さず、VMが自分でタグを付けて別VLANへ到達することも許さないため

nftaclでは、2本目のbase chain（`bridge kyuusha_acl antispoof`、`hook prerouting`、
`policy accept`）がtapごとに`iifname "<tap>" jump <tap>-spoof`を持つ。forwardではなく
preroutingに置くのは、VMからブリッジ自身（`gateway_ip`、つまりホストがルーティングする
先すべて）宛てのフレームはローカル配送されてforwardフックを通らないため——forwardだけで
検査すると、ルーティングされる経路の詐称が素通りになる。

`ip_address`/`mac_address`のどちらかが空のattachは「アドレス不明」として扱い、既に
入っているアンチスプーフィングのチェーンには触らない（空にしない）——再適用はルールを
変えるだけでアドレスは変えないので、「不明」が「検査をやめる」になってはならないため。

- 実装済み・playground実機確認済み（既定のSecurityGroupでのNetwork内の疎通、
  グループを外した時の拒否、メンバーの増減の反映を確認——詳細はdocs/release-notes.md参照）。
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

### 単体のSNAPバイナリとしてのnftacl: `cmd/nftacl-snap`

同じnftacl実装（アンチスプーフィング・ルーティングされる通信への適用込み）を、SNAP契約を
満たす単体のバイナリとしても提供する。compute-agentのコンテナイメージに
`/usr/local/bin/nftacl-snap`として同梱しており、`-security-backend-bin=/usr/local/bin/nftacl-snap`
を指定しても未指定（組み込み）と同じ動作になる。主な用途は、独自のSNAP shimが一部の
NetworkInterfaceだけを従来どおりのnftaclへ委譲すること（execしてpayloadをそのまま渡す）。
payloadの解釈は`internal/compute-agent/snap`の`PluginRequest`/`ServeBuiltin`を
組み込み実装と共有している。

## 非ブリッジ配線向けの参考実装: `examples/snap-plugins/ebpf-snap`

`nftacl`はtapがLinuxブリッジのポートであることを前提とするため、VNAPで非ブリッジ
配線（`examples/vnap-plugins/frr-vrf-host-route.sh`/`frr-ipv4-unicast.sh`のような
pure L3構成）を使う場合は
`-security-backend-bin`で別のSNAP実装を組み合わせる必要がある、と上で述べた。その
具体例として、TC-BPF（tapデバイスのclsact ingress/egress両フックに`cilium/ebpf`で
直接アタッチ、ブリッジのポートである必要が無い）によるステートフルな参考実装を
`examples/snap-plugins/ebpf-snap/`に用意した——VNAPの`examples/vnap-plugins/`と
同じ「アダプトして使う参考実装」という位置づけ（本体のcompute-agentイメージ・
ビルドには組み込まない、独立したGoモジュール）。

netfilterのconntrackが使えないTC-BPFフック向けに、自前の正規化5-tupleベースの
conntrack相当（BPFの`LRU_HASH`マップ、全tap共有）を実装しており、`nftacl`の
`ct state established,related`と同等の双方向ステートフル動作を実機（veth
ペア+network namespaceでの実トラフィック）で確認済み。詳細・設計判断・実機確認結果は
`examples/snap-plugins/ebpf-snap/README.md`参照。性能重視のステートレス版は
別途後日の課題。

上記のアンチスプーフィングも同じ規則で実装している（tapのTCX ingressで、ACL判定より
前に検査する。期待するIP/MACはtapごとの`spoof`マップに入る）。アドレス集合はホストで
1つの共有ハッシュマップ（`set_members`、キーは(集合名から求めた32bitのid, アドレス)）で、
ルールは集合のidかCIDRを持つ。IPv4だけを強制する（IPv6のCIDRのルールは無視する）。

### 実トラフィックによる検証

nftacl・ebpf-snapとも、network namespace＋vethで2台のVMを模した実トラフィックの
特権テストを持つ（nftaclはさらに、専用のnetns内で2つのSubnetのブリッジと
`ip_forward=1`を用意し、ホスト経由のルーティングにルールが効くことを確かめる
`nftacl_routed_test.go`も持つ。`internal/compute-agent/nftacl/nftacl_traffic_test.go`、
`examples/snap-plugins/ebpf-snap/antispoof_test.go`。rootでないとskip）。正規の
VM間・VM→ゲートウェイ通信が通ること、送信元IP・送信元MAC・ARP送信者IPの詐称が
相手側に届かないことを、相手側network namespaceのnftablesカウンタで確認する。アドレス集合も実トラフィックで
確かめる（nftaclは`network:<id>`の集合で許したVM同士の通信、ebpf-snapは集合への追加・
削除・全量の置き換えに応じて相手からの着信が通る/通らないこと。
`examples/snap-plugins/ebpf-snap/sets_test.go`）。

## ポリシーの配布

稼働中のVMのNICのポリシーは、compute-agentがnetworkサービスに張るgRPCの双方向ストリーム
（`kyuusha.network.agent.v1.PolicyDistributionService.Stream`、東西mTLS、api-gatewayは
中継しない）で追従させる。EnvoyのxDSと同じ形で、設計の理由は
[architecture.md「ポリシーの配布: xDSのようなgRPCストリーム」](../architecture.md)。
compute-agentの`-network-addr`で接続先（networkのどのレプリカでもよい）を指定する。
空ならストリームを張らず、NICは起動時のポリシーのまま（警告をログに出す）。

### やりとり

| 向き | メッセージ | 中身 |
|---|---|---|
| agent→network | `Subscribe` | `hypervisor`と、そのホストのドライバが配線済みのNICの一覧。**申告し直すたびに置き換える**（state of the world）。ストリームの最初のメッセージは必ずこれ |
| network→agent | `PolicyStreamResponse` | `nonce`（ストリーム内で増える）、`revision`（networkのキャッシュが反映しているetcdリビジョン）、`observed_at`、`interfaces`（NICごとのポリシーの全量）、`sets`（集合の全量または差分） |
| agent→network | `Ack` | 応答の`nonce`と、適用できなかった場合は`error`（NACK） |

networkが送るもの:

| きっかけ | 送る先 | 中身 |
|---|---|---|
| NICが新しく申告された | そのストリーム | そのNICのポリシーの全量と、そのストリームにまだ送っていない集合の全量 |
| NICのアドレス・付いているグループ（`SetSecurityGroups`）が変わった | そのNICを申告しているストリーム | そのNICのポリシーの全量（新しく参照する集合があればその全量も） |
| グループのルールが変わった | そのグループが付いたNICを申告しているストリーム | それらのNICのポリシーの全量 |
| 集合のメンバーが変わった（NICの払い出し、VMの起動・停止・削除・移行、付け替え） | その集合の写しを送ってあるストリーム | 差分（`add`/`remove`、版はその変更のetcdリビジョン） |
| 接続直後・networkのキャッシュの作り直し・5分ごと | 全ストリーム | 申告されている全NICのポリシーと、必要な全集合の全量 |

### compute-agentの側

- 起動時と、ドライバの配線状態が変わった時（VMのBoot直後、それ以外は1秒ごとに確認）に
  `Subscribe`を送り直す
- 応答を受け取ると、まずNICごとのポリシーを該当tapのドライバの`ApplyACL`で適用し
  （SNAPの`attach`。新しく参照された集合はここで空として作られる）、次に集合の更新を
  SNAPの`update_sets`で適用して、`Ack`を返す。どれかが失敗したら`error`付きで返す
- NICのポリシーは、そのNICに最後に適用したものより古い版なら捨てる（同じ版の再送は
  そのまま適用し直す）。集合の版の扱いは上記「版」
- ストリームが切れたら、ジッター付きの待ち時間（1秒から倍々で最大30秒）で繋ぎ直し、
  全量から受け取り直す。networkのレプリカの再起動で全ホストが一斉に繋ぎ直さないため
- VMの起動時のtapの配線には、computeが読んだポリシー（`GetSecurityPolicy`）を使う。
  VMは最初のパケットから正しいルールで動き、ストリームはその後を追従させる

### networkの側

networkのAPIの各レプリカが`PolicyHub`（`internal/network/policyhub.go`）を持ち、
NetworkInterfaceとSecurityGroupを一覧してから、そのリビジョンからWatchしてキャッシュを
保つ。Watchが切れたら一覧し直し、全ストリームへ全部を送り直す。共有の状態を持たないので、
レプリカの数に制約は無い。ストリームの送信が溜まりすぎた（1024応答）場合はそのストリームを
切り、agentに繋ぎ直させる。

### 反映の遅れと適用状況

反映は最終的に揃う方式で、新しいVMのアドレスが他のホストの集合に入るまでの間は、そのVMとの
通信が相手側で拒否されうる。目標は数秒以内。

- `kyuusha_compute_agent_sg_set_propagation_seconds`（compute-agent、ヒストグラム、
  `kind`=`delta`/`full`/`policy`）: networkが変化を見てから、そのホストで適用し終える
  までの時間
- `kyuusha_network_policy_revision`（network）と`kyuusha_network_policy_acked_revision`
  （`hypervisor`別）: キャッシュの版と、各ホストが適用できたと返した最新の版。差が
  開いたままのホストは追従できていない
- `kyuusha_network_policy_nacks_total`（`hypervisor`別）: 適用できなかった応答の数
- `kyuusha_network_policy_streams`（`hypervisor`別）: 繋がっているストリームの数
- `kyuusha_network_sg_set_updates_total`（`kind`別）: 送った集合の更新の数
