# SNAP（Security Network Attach Protocol）仕様

`ingress_rules`（VMへの着信を許可するルール）/`egress_rules`（VM発の送信を許可する
ルール）は、VNAP（[VNAP仕様](vnap.md)参照）と同じ設計思想でプラガブルにしたホスト側
ACL強制プラグイン契約——SNAP（Security Network Attach Protocol）——によって実際に
強制される。`-security-backend-bin`（compute-agentのフラグ）を指定すると、tapの
ワイヤリング（VNAP）とは独立に外部バイナリへ委譲できる——未指定（既定）なら
`internal/compute-agent/nftacl`（後述）が担う。VNAPと1つのプラグインに統合していない
理由: 配線（tapをどのスイッチに繋ぐか）とACL強制（何を通すか）は直交する関心事で、
片方だけ差し替えたい運用（例: 既定のLinuxブリッジ配線のままeBPF/OVS ACLだけ独自実装
に差し替える）に対応するため。

- **契約はVNAPと同型**（`<bin> attach`/`<bin> detach`をexec、標準入力にJSON、成否は
  exit codeのみ、タイムアウト10秒、プラグイン側が冪等性の責務を負う）——ただし
  ペイロードの中身もフラグも別（`-network-attach-bin`とは無関係）
- **attachのpayload**: `tap_name`/`iface_id`/`vm_id`/`tenant_id`/`subnet_id`/
  `subnet_cidr`/`gateway_ip`/`ip_address`/`mac_address`/`ingress_rules`/`egress_rules`
  ——`ip_address`/`mac_address`はそのVMに払い出された自身のアドレスで、アンチ
  スプーフィング（後述）の入力。`UpdateFirewallRules`後の再適用でも毎回同じ値が
  届く（networkサービスが`update_acl`コマンドに載せる）。プラグインは知らない
  フィールドを無視すること
- **呼び出しタイミング**: VM Boot時（`netsetup.Wire`成功直後）と、`UpdateFirewallRules`
  呼び出し後の再適用時（後述、NATS経由）の両方
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
  もう一方のチェーンが評価されなくなってしまう）。`drop`で明示的な拒否、チェーン末尾にも
  `drop`（該当ルール無しはデフォルト拒否）。両チェーンをreturnで通過した後、実際に許可
  するのはbase chain自身の`policy accept`
- `<tap>-in`/`<tap>-out`はどちらもARP（`ether type arp`）を常に通す——ARPはIPv4では
  ないので`ip saddr`/`ip daddr`のベースラインに一致せず、conntrackも追跡しないため、
  通さないと同じブリッジ上のVM同士がそもそも互いのMACを解決できない。ARPの正当性の
  検査は下記のアンチスプーフィングが担う

### アンチスプーフィング

`ingress_rules`/`egress_rules`とは独立に、VMが送信する全フレームについて次を強制する
（テナントが設定で緩めることはできない）:

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
前に検査する。期待するIP/MACはtapごとの`spoof`マップに入る）。

### 実トラフィックによる検証

nftacl・ebpf-snapとも、network namespace＋vethで2台のVMを模した実トラフィックの
特権テストを持つ（`internal/compute-agent/nftacl/nftacl_traffic_test.go`、
`examples/snap-plugins/ebpf-snap/antispoof_test.go`。rootでないとskip）。正規の
VM間・VM→ゲートウェイ通信が通ること、送信元IP・送信元MAC・ARP送信者IPの詐称が
相手側に届かないことを、相手側network namespaceのnftablesカウンタで確認する。

## `UpdateFirewallRules`とホストへの反映

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
