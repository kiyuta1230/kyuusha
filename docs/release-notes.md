# リリースノート

このプロジェクトの「いつ・何が変わったか」の記録。**設計判断の「なぜ」は
[docs/architecture.md](architecture.md)、現状の仕様は[docs/specs/](specs/README.md)を
参照**——ここには日付付きの事実のみを置き、設計トレードオフの深掘りはarchitecture.mdへ
リンクする形にする。

## 2026-10-03

- **既定のSNAP実装（nftacl）とebpf-snap参考実装にアンチスプーフィングを追加した**
  （kyuusha-vpcからの変更依頼B1）。VMが送信するフレームについて、送信元MAC＝払い出された
  MAC、IPv4の送信元IP＝払い出されたIP、ARPの送信者MAC/IP＝自身のもの（`0.0.0.0`のprobeは
  可）を強制し、それ以外のEtherType（IPv6・802.1Qタグ付き）はdropする。nftaclでは
  bridge preroutingフックの専用チェーン、ebpf-snapではTCX ingressでACL判定の前に検査する
  （[SNAP仕様](specs/snap.md)「アンチスプーフィング」）。修正前のコードに対して
  network namespace＋vethの実トラフィックテストを流し、**同一Subnetの他VMのIPを名乗った
  パケットも、偽のMACで送ったフレームも、実際に相手VMへ届くこと**を確認したうえで修正した
- **nftaclが同一Subnet内のVM間通信を常に落としていた実バグを発見・修正した**:
  `<tap>-in`/`<tap>-out`チェーンにARPを通すルールが無く、ARPがチェーン末尾の`drop`に
  落ちていたため、同じブリッジ上のVM同士が互いのMACを解決できなかった（VM→ゲートウェイは
  forwardフックを通らないので影響が無く、ゲストのゲートウェイpingによる自己診断では
  気づけなかった。これまでのnftaclの確認は`nft list ruleset`のテキスト確認だけで、
  VM間の実トラフィックは試していなかった）。上記と同じ実トラフィックテストで発見。
  ARPを常に通し、正当性はアンチスプーフィング側で検査する形にした
- **VNAP/SNAPのattachペイロードを拡張した**（変更依頼A2、追加のみで後方互換）:
  VNAPに`subnet_id`/`zone`/`subnet_cidr`、SNAPに`subnet_id`/`ip_address`/`mac_address`。
  プラグインが(zone, vlan_id)からSubnetを逆引きする必要が無くなった。
  `UpdateFirewallRules`後の`update_acl`コマンドにも`subnet_id`/`ip_address`/
  `mac_address`を載せ、再適用でもアンチスプーフィングを作り直せるようにした。
  playgroundで実VMを起動し、ゲストのゲートウェイ疎通がアンチスプーフィングのチェーンを
  経由して通ること、`UpdateFirewallRules`でチェーンが正しいIP/MACで再構築されることを確認

- **ネットワーク接続パターンを4分類に整理し、VNAP参考実装/containerlabラボの命名・
  ディレクトリを全面的に付け替えた**（元の「Type-2/Type-5」というEVPN route type
  由来の語彙から、「L2 VLAN」「L3 Pure L3（IP一意/IP重複許容）」という接続特性
  ベースの語彙へ）:
  - `examples/vnap-plugins/frr-type5.sh` → `frr-vrf-host-route.sh`（`git mv`で
    履歴保持、中身は無変更）
  - `playground/containerlab-clos/` → `playground/vlan-clos/`、
    `playground/frr-type5-clos/` → `playground/evpn-vxlan-clos/`
    （いずれも`git mv`、中身は無変更）
- **新規VNAP参考実装`examples/vnap-plugins/frr-ipv4-unicast.sh`を追加**
  （pure L3・IP一意デプロイ向け）。`frr-vrf-host-route.sh`と異なりVRFを一切使わず、
  ハイパーバイザが`gateway_ip`をSubnetの実prefix長で持つ本物のL3ゲートウェイになる
  （proxy ARP併用）。ハイパーバイザ自身もLeafとL3接続しデフォルトルートを
  `default-originate`で受け取る構成を前提にする。新規ラボ
  `playground/ipv4-unicast-clos/`（containerlab、unnumbered eBGP、
  VRF/EVPN/VXLAN無し）でホスト跨ぎの実機確認を行い、ハイパーバイザが実際に
  Leaf発のデフォルトルートを学習しそれが機能することまで確認した
- **新規ラボ`playground/vrf-lite-clos/`を追加**（`frr-vrf-host-route.sh`を無改造の
  まま使い回す）。`playground/evpn-vxlan-clos/`（BGP EVPN Type-5 + VXLAN）との
  違いは網側の実現方式のみ——VRFスコープの素の`address-family ipv4 unicast`
  eBGPで経路を運び、EVPN/VXLANは一切使わない。host-side VNAPロジックが
  EVPN+VXLANとVRF-liteの両方の網側実現方式から無改造で使い回せることを実機で
  確認した（**host-leaf間のBGP技術選択とホスト側VNAPの責務は直交する**、という
  設計原則をディレクトリ構成自体が体現する形にした）。このラボの構築中に
  **FRR 10.5.1の既知の制約**（unnumbered eBGPが非デフォルトVRFインスタンス内では
  確立しない）を確認し、numbered（ポイントツーポイントアドレス方式）eBGPに
  切り替えた
- **`frr-ipv4-unicast.sh`/`frr-vrf-host-route.sh`の実バグを発見・修正した**:
  両スクリプトとも、VM自身の`/32`をFRRへvtysh経由のstatic routeとして注入する
  のに加えて、カーネルへも直接`ip route replace`で同じ`/32`を入れていたが、
  これがzebraの経路選択で**FRRの"S"（static）routeより優先される"K"（kernel）
  routeとして扱われ**、static routeが選択経路(best path)にならず
  `redistribute static`が発火しない（=経路が他ホストへ一切広報されない）という
  実機でしか分からない不具合だった。`playground/ipv4-unicast-clos/`の構築時に
  発見し、両スクリプトから該当のカーネル直接操作を削除——FRR自身が選択した
  static routeをカーネルFIBへ自動的にインストールするため、この手動操作は
  そもそも不要だった
- 上記に伴い、`examples/vnap-plugins/README.md`・`docs/specs/vnap.md`
  「参考実装」節・`docs/network-deployment-guide.md`「3.5. Pure L3デプロイの
  場合」（旧「3.5. Type-5（EVPN pure L3）デプロイの場合」）・
  `playground/README.md`「手動検証ツール」・`.gitignore`のcontainerlab
  lab-stateディレクトリ除外エントリを、新しい3スクリプト・4ラボ構成に合わせて
  更新した。4ラボ全て、リネーム後・新規追加後に`containerlab deploy`から
  実機で再検証済み

## 2026-09-28

- **Public IP Attach（floating IP相当）を`Subnet`の機能として実装**（新しいリソース
  種別は増やさない）。`SubnetSpec`に`unique_cidr`（trueなら他の`unique_cidr=true`な
  Subnetとの`cidr`重複を全テナット横断で拒否）と、`kyuusha.image.v1.ImageSpec`と
  同じ意味の`visibility`(PRIVATE/PUBLIC)・`shared_with_tenant_ids`
  （所有テナント以外に実際に`NetworkInterface`をattachしてよいテナントIDの許可
  リスト）を追加した。`CreateNetworkInterface`は呼び出しテナント自身が所有しない
  Subnetも全テナント横断で解決できるようになり（`getSubnetForInterface`）、
  `subnetUsableBy`で利用可否を判定する。詳細は[network仕様](specs/network.md)
  「`spec.unique_cidr` / `spec.visibility` / `spec.shared_with_tenant_ids`」参照
- **破壊的変更**: `SubnetSpec.shared_with_tenant_ids`（旧field 5、
  「他テナントが自分のingress_rules/egress_rulesの中でこのSubnetのCIDRをallow
  宛先として名指ししてよいか」というACL参照専用の同意フィールド）を削除した
  （`validateCrossTenantRules`ごと削除）。同じフィールド名を上記の新しい意味
  （field 11）で再利用している。同等以上の制御が必要になった場合は、既存の
  `internal/admissionwebhook`（現状`VirtualMachineService.Create`のみに配線済み）
  を`network`サービスへ配線する方針にした（`docs/architecture.md`「networkサービス
  のリソース: Subnet / NetworkInterface」参照）
- CLI: `kyuusha subnet create`に`-unique-cidr`・`-visibility`・
  `-shared-with-tenant-ids`（新しい意味）を追加
- **`spec.visibility=PUBLIC`は`spec.unique_cidr=true`を要求するよう追加でバリデーション
  した**（`CreateSubnet`/`UpdateSubnet`、`ErrValidation`）。上記実装直後に
  `docs/architecture.md`「テナント間でのSubnet共有」の既存原則（無目的なL2共有はしない）
  と矛盾していることに気付いたための追加ガード——所有テナントが相手を検証しない無条件の
  オープン共有（`visibility=PUBLIC`）はPublic IP用アドレス空間限定にし、所有テナントが
  個別に名指しする`shared_with_tenant_ids`（マネージドサービスの顧客Subnetへの直接注入
  等、目的のある共有）にはこの制約を課さない、という整理に合わせて
  `docs/architecture.md`の当該節を書き直した
- **ロードマップPhase 04の残り項目のうち「VLANプール枯渇時のVXLANへのエスケープパス」を
  不要と判断、クローズした**: VNAPが既にホストを跨ぐ実現方式をプラガブル化しており、
  Type-5（EVPN pure L3）はそもそも「1 VLAN = 1 VRF」に依存しないためVLANの4094上限が
  問題にならない。4094を超えるAZは新規にVXLANハイブリッドを実装するのではなく、その
  AZをType-5デプロイへ切り替えることで対応する、という既存の選択肢で足りると結論づけた。
  `docs/architecture.md`「制約: VLANの4094上限はType-2デプロイの宿命として受け入れる」
  （旧「制約と将来のエスケープパス」）・`docs/network-deployment-guide.md`「1. VLAN
  プール設計」を、この結論に合わせて書き直した（旧記述は「将来的なVXLANへのエスケープ
  パスが用意されている」と実装が無いのに用意されているかのように書いていた点も訂正）
- **ロードマップPhase 04の残り項目「マルチホストL2の標準実装が無い」のType-2側を
  クローズ**: 新規参考VNAPプラグイン`examples/vnap-plugins/vlan-trunk.sh`を追加した。
  組み込みのLinuxブリッジ実装（ホスト内のみ）に、アップリンクNICへの802.1Qタグ付き
  VLANサブインターフェース作成を加え、実際のホスト跨ぎL2疎通を実現する——スイッチ側の
  ゲートウェイ実装（伝統的なコア/ToRのSVIか、CLOSファブリックのいずれかのスイッチが
  担当するか）はどちらでも変わらず機械的に同じ手順のため、`frr-type5.sh`と違い
  「そのまま使える」参考実装として成立する。組み込み実装自体（`-network-attach-bin`
  未指定時）は変更していない。Type-5側は、FRR/BGP設定が環境ごとに大きく異なる
  プロトコルレベルの事情を抱えるため、引き続き「読んで自分の環境に合わせて作り込む」
  参考実装のままとする（クローズしない）。実機確認方法: 当初`compute-agent-1/2/3`が
  共有するplayground既定のDocker bridgeネットワーク上でVM間pingを試みたが、ホストの
  ブリッジがアップリンクポートから受信したブロードキャストをtapポートへフラッドしない
  という原因不明の現象に阻まれた（`vlan_filtering`/`mcast_snooping`/netfilter/STP/
  MACアドレス衝突は全て切り分け済みで原因特定に至らず）。改めてcontainerlab製の
  leaf-spine-leaf CLOS疑似ファブリック（本物のVLAN-aware Linuxブリッジがスイッチ役、
  `vlan-trunk.sh`本体をhostノード上でそのまま実行）で検証したところ、4ホップ越しの
  ping疎通に完全に成功し、スクリプト自体の設計は正しいことを確認した。あわせて
  最小構成（2コンテナが同一のDocker bridgeネットワークを直接共有する構成、
  マルチアクセスL2・重複したgateway_ip割り当ての2条件でも再現するかを個別に検証）
  でも問題なく疎通したため、最初の失敗は上記playground環境（長時間稼働させながら
  手動でのブリッジ設定変更を繰り返した1コンテナ）固有の何らかの状態に起因するものと
  みられる——スクリプトの設計・実装自体の欠陥ではないと判断した
- **`examples/vnap-plugins/frr-type5.sh`の実バグを発見・修正した**: `vlan-trunk.sh`と
  対になる形で、containerlab製の本物のBGP EVPN Type-5ラボ（`playground/frr-type5-clos/`、
  leaf-spine-leafの4ホップ、host1/host2それぞれ個別ASNのunnumbered eBGP、
  VXLANカプセル化あり）を新規に組み、`frr-type5.sh`をそのまま実行して初めてホスト跨ぎの
  実機検証をした。その結果、**tapデバイスをテナントのVRFへ`master`として所属させる
  処理が漏れていた**バグが見つかった——このため、スクリプトが`vtysh`経由でFRRへ注入する
  static routeはFRR側の設定としては受理されるように見えても、カーネル/RIBへ実際には
  一切インストールされない（`ip link set $tap master $vrf`が無いと、Linux kernelの
  VRFルーティングテーブルは出力先デバイスがそのVRFのメンバーでないルートを解決できない
  ため）。この状態ではType-5は実質全く機能していなかった。`ip link set "$tap" master
  "$vrf"`を追加して修正し、host1↔host2間で実際にVXLANカプセル化を経由した双方向ping
  （0%ロス）を確認した。あわせて、ネットワークチーム側の責務であるFRR設定にも
  ドキュメント化されていなかった前提（L3VNIが`State: Up`になるには、実データ疎通が
  無くてもSVI＝ブリッジが要る/そのSVI自体もVRFへ`master`所属が要る/`advertise-all-vni`
  がVNI認識に要る/`redistribute connected`で各VTEPのloopbackへの到達性を確保する必要が
  ある）が複数見つかったため、スクリプト自身の「Required companion FRR config」節に
  追記した
- **グラフィカルコンソール（VNC/SPICE相当）を見送りと判断、クローズした**:
  Firecrackerは設計上VGA/GPUエミュレーションを持たず、Cloud Hypervisor（kyuushaが
  固定するv53.0含め現行の公式リリース全て）もvirtio-gpu/VNCを公式に持たない
  （[upstream issue](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/3212)
  はclosed、[Spectrum OSの非公式パッチ](https://spectrum-os.org/software/cloud-hypervisor/)
  のみ存在）ため、両VMMバックエンドの上流に起因する制約と判断した。パッチ済みCHの
  採用（無改造アップストリームバイナリ方針からの逸脱）・QEMUを3つ目のVMMドライバに
  する（バックエンドエコシステムの増殖）のどちらのコストも、実際のユースケースの
  狭さに見合わないと判断し、見送りを選択。`docs/architecture.md`「未決事項」から
  「解決済み」へ移動した

## 2026-09-27

- `docs/network-deployment-guide.md`に2点追記した（ロードマップPhase 04
  「サポートするトポロジの組み合わせを明文化」の一部）:
  - 「全体像」直後に新節「VLAN到達範囲はアンダーレイ構成に依存する」を追加し、
    (a)伝統的なコア/ToR構成・(b)CLOS(オーバーレイ無し、ラック単位でVM networkが
    分断される)・(c)CLOS+VXLAN(本ガイドの既定の前提、AZ全体でストレッチ)の
    3パターンを図示して整理した。あわせて、kyuushaのスケジューラは現状
    「あるSubnetのVLANがそのスケジュール候補ホストへ実際に届いているか」を
    一切見ない（ネットワークチーム側の前提条件として一切保証されない）ことを明記
  - 「3.5. Type-5」節に、host-Leaf間の推奨参照構成（unnumbered eBGP、ASNは
    ハイパーバイザ1台ごとに個別payout、想定台数に応じた2-byte/4-byte ASN幅の
    選び方）を追記した。あくまで参照構成の推奨であり、
    `examples/vnap-plugins/frr-type5.sh`自体はBGPセッションの設定
    （ASN・eBGP/iBGPどちらか含め）に一切関与しないことも明記——実際に
    スクリプトを読み返して確認した（プラグインはFRRのRIBへのstaticルート
    注入のみ行い、`router bgp`設定はコメントのcompanion sketchとして
    示すだけで実行はしない）

- `docs/rolling-upgrade.md`を新設し、コントロールプレーンのローリング
  アップグレード手順を文書化・実地確認した（本番化ロードマップPhase 03最後の
  項目）。API面（ステートレス複製、真のローリング）・reconcile面
  （常に1インスタンス、recreate方式）・compute-agent（ホストごとの
  in-placeバイナリ入れ替え）で手順が異なることを明記し、wireプロトコルの
  互換性ポリシー（protoフィールドは追加のみ）も定めた。

  実地確認: playgroundで(1) VMを`Pending`のまま`compute-reconciler`を
  停止→確認→再起動し、API面は無停止のまま応答し続け、reconciler復帰後に
  そのVMが自然に`Running`まで進むこと、(2) `vm get`を継続ポーリングしながら
  `compute`（API面）をrecreateし、短い接続断（約15秒）の後に状態欠落なく
  応答が再開すること、を確認した。

  副産物: `internal/compute-agent/fcvmm`/`chvmm`の`Reconcile`（compute-agent
  プロセス再起動を跨いで実行中のVMプロセスを再認識する、in-placeアップグレード
  の前提となる機構）に、実装以来初めてテスト（`TestManagerReconcileAdoptsRunning
  ProcessAcrossRestart`）を追加した——実Firecracker/cloud-hypervisorを使わず、
  スタンドインの長命プロセス（`sleep`）で同じ検証ができることを確認。

  判明した現実的な制約: 現状の`docker/Dockerfile`のcompute-agentステージは
  バイナリ自身がコンテナのPID 1（supervisor無し）で、コンテナを作り直さずに
  バイナリだけをin-place入れ替える手段が無い（コンテナ作り直しはネットワーク
  名前空間ごと破棄し、そのホスト上の全VMを道連れに終了させる——
  `docs/specs/snap.md`で既知の挙動）。ベアメタル/systemdデプロイなら
  `systemctl restart compute-agent`で済むが、コンテナ化デプロイでの
  in-placeアップグレードには軽量supervisorの導入という構成変更が要る
  （`docs/open-questions.md`「compute-agentコンテナへのsupervisor導入」に
  意図的な先送りとして記録、実際にコンテナ化デプロイでの需要が出た時点で着手）。

- `playground/etcd-failover-test.sh`を追加し、実3メンバーetcdクラスタでの
  リーダー障害を実際に検証した（本番化ロードマップPhase 03「実ノード障害での
  reconcilerフェイルオーバー検証」——ただしetcd自体のRaft正しさではなく、
  kyuusha自身のetcdクライアント/reconcileループが実際のリーダー障害から
  復帰するかに絞った検証。詳細な理由は`docs/architecture.md`「正直な残課題:
  etcdクラスタ自体の冗長化はデプロイ環境側の前提」参照）。
  `playground/docker-compose.yml`は`-etcd-endpoints`を`${KYUUSHA_ETCD_
  ENDPOINTS:-etcd:2379}`という変数展開に変え（未設定なら従来と完全に同じ
  単一メンバー構成）、新設の`playground/docker-compose.etcd-cluster.yml`
  （オーバーレイ、単体では使わない）と組み合わせて初めて3メンバー構成になる。
  実際にリーダーのコンテナを`docker kill`（優雅な停止ではなく実ノード喪失を
  模擬）し、6秒後に残り2台で新リーダーが選出されること、その間`compute`/
  `network`/`block-storage`の各reconcilerプロセスが生き続けること、リーダー
  交代を挟んで新規に作成したVirtualMachineが問題なくスケジュール・実
  Firecracker起動まで完了すること、リーダー障害の前から張っていた
  `kyuusha vm watch`のWatchストリームが再接続無しに配信を継続することを
  実機で確認した——全てPASS。CIには含めない（実Raft選挙という数秒単位の
  タイミング依存な検証で、共有CI runnerでの実行に向かないため）。

- quota/スケジューラの並行性・負荷テストを追加した（本番化ロードマップPhase 02の残り、
  `docs/architecture.md`「未決事項」4番目参照）。`internal/compute/concurrency_test.go`に
  2つ追加:
  - `TestService_ConcurrentCreateNeverOverchargesTenantQuota`（通常の`go test ./...`で
    実行される、決定的な正しさのテスト）: 厳しい`max_vms`Quotaに対して大量の`Create`を
    並行実行し、成功数がQuota通りであること・`tenant_usage`と実際にetcdへ永続化された
    VM数が一致することを確認する。`hypervisor_service_test.go`の既存の容量レースは
    `reserveHypervisorCapacity`を直接叩くもので、`Service.Create`自体・`usageMu`自体を
    通した並行テストは今回が初めて
  - `BenchmarkService_CreateUnderQuotaContention`（`-bench`を明示しない限り実行されない
    ベンチマーク、CIには含めない——結果がスループット特性であって正誤ではなく、共有CI
    runnerのハードウェアはしきい値判定に使えるほど安定していないため）: `-cpu=1,4,16`で
    実測したところ、並行数を上げてもns/opがほぼ変わらない（4.7ms→5.4ms→5.5ms）ことを
    確認した——`usageMu`が全テナント共通の単一ロックである設計（`Service.Create`が
    image/subnet/volume検証・identityへのQuota同期取得・admission webhook呼び出しまで
    ロック内で行う）が実際にスループットを頭打ちにすることを実測で裏付けた
  - `internal/resourcetest.Client`の引数型を`*testing.T`から`testing.TB`へ広げ、
    `*testing.B`からも同じ埋め込みetcdフィクスチャを使えるようにした（既存呼び出し
    箇所は`*testing.T`のまま、`testing.TB`を満たすので無変更で動く）

- `.github/workflows/ci.yml`に`playground-e2e`ジョブを追加し、
  `playground/scenario.sh`（実Firecracker VM起動を含む多ハイパーバイザーE2E）を
  push/PRのたびに自動実行するようにした（`docs/open-questions.md`
  「playground/scenario.shのCI自動実行をやるべきか」の解消）。GitHub-hosted
  `ubuntu-latest` runnerに一時的な診断ステップを追加して実際に確認した結果、
  `/dev/kvm`自体は存在し（runner自身のユーザーは`kvm`グループに属さないため
  素のままでは使えないが、パスワード不要sudoでの`chmod`で解消可能）、加えて
  playgroundの各compute-agentコンテナは`privileged: true`で動くため、そもそも
  ホスト側のグループ権限に関係なくデバイスアクセスできることも判明した——
  self-hosted runnerは不要だった。診断ステップは役目を終えたので削除済み。

- root disk転送のストリーミングpush化を実装した（`docs/open-questions.md`
  「ルートディスク転送のストリーミングpush化」の解消）。
  `internal/compute-agent/imagestore.PushOCIBlob`（`Migrate(transfer_root_disk=
  true)`用）と`cmd/kyuusha/imagebuild.go`の`orasPushFile`（`kyuusha image
  build`用）はどちらも、対象ファイルを`[]byte`として丸ごとメモリへ読み込んで
  から`oras.PushBytes`へ渡す実装だった——両方とも、oras-go/v2の低レベルAPI
  （`os.File`に対して1回streamingでdigestを計算し、`Seek(0, io.SeekStart)`で
  巻き戻してから、事前計算済みの`ocispec.Descriptor`と一緒に
  `content.Storage.Push`へそのまま渡す2パス方式）へ書き換え、ファイル全体を
  メモリ上の`[]byte`として保持する瞬間を無くした。マニフェストのpack/tag部分
  （`oras.PackManifest`/`repository.Tag`）は変更していない。

  テスト: `internal/compute-agent/imagestore`に、push+PUT両対応のOCI
  Distribution APIスタブ（`newPushableTestOCIRegistry`）を新設し、実際に
  `PushOCIBlob`でpushしてから`fetchOCIBlob`で読み戻す往復テスト
  （`TestPushOCIBlobStreamsFileRoundTrip`、~4.2MBのファイルで検証）を追加、
  全て通過を確認。

  playground実機検証: 実VMを起動→Stop→`vm migrate -transfer-root-disk`で
  別Hypervisorへ実際に転送し、新Hypervisor側で正しくRunningへ復帰することを
  確認。`kyuusha image build`も実registryへストリーミングpushし、Imageが
  Readyへ到達、そこからVMが実際に起動することを確認。

  実機検証で見つけた別のバグ（今回のストリーミング化とは無関係、既存の
  マイグレーション実装の潜在バグ）: `internal/compute/reconciler.go`の
  `migrateVM`が、root disk転送成功後の最終`Update`（`Phase=Scheduled`と
  `PendingRootDiskURL`を書き込む）で`resource_version`競合に遭遇すると、
  新Hypervisorの容量予約は解放するのに、直前に成功していたはずのpushした
  アーティファクトを削除しないまま放置していた——`PendingRootDiskURL`が
  一度もetcdへ永続化されないため、`handleCreateResult`側の通常のクリーン
  アップ経路が対象URLを一生知り得ず、レジストリに永久に残り続ける
  （次のリトライで別タグへの再pushも発生するため二重消費にもなる）。
  この`Update`失敗パスでも同じ削除コマンドをその場で送るよう修正した。

- `.github/workflows/ci.yml`を新設し、初めてCIパイプラインを整備した（これまで
  playground手動実行のみが検証手段だった）。ルートモジュール（`go build`/
  `go vet`/`gofmt -l`/`go test`、および`buf generate`の生成差分チェック）と
  `examples/snap-plugins/ebpf-snap`（別Goモジュールのため別ジョブ、`cilium/ebpf`の
  依存がルートに波及しないことも含めてCIで担保）の2ジョブ構成。`CAP_NET_ADMIN`/
  `/dev/kvm`が要る実機系テスト（`internal/compute-agent/netsetup`/`nftacl`等）は
  既存の「権限が無ければ自発的にskip」規約のまま、通常のGitHub Actionsホスト
  runnerで無改造のまま動く。playground/scenario.shの自動実行（実Firecracker起動を
  含むE2E）は今回のスコープ外——KVM可用性が不確実なホストrunnerでは実質的な検証
  価値が下がるため、別途self-hosted runnerの要否とセットで判断する
  （`docs/open-questions.md`に積む想定）。

- `shared_with_tenant_ids`によるクロステナントCIDR許可の検証と、`mesh_group`が
  一致するSubnet同士の自動許可を実装（`docs/open-questions.md`に積んでいた見送り
  2項目の解消）。前者は`internal/network/firewallrule.go`の
  `validateCrossTenantRules`——`ingress_rules`/`egress_rules`の`allow`ルールが
  他テナントのSubnet CIDRへ重なる場合、対象Subnetの`shared_with_tenant_ids`に
  自テナントが含まれていなければ`CreateNetworkInterface`/`UpdateFirewallRules`を
  `ErrValidation`で拒否する（`deny`ルールと自テナント宛は対象外）。後者は
  `Service.EffectiveFirewallRules`——対象NetworkInterfaceのSubnetが`mesh_group`を
  持つ場合、同一テナント・同じ`mesh_group`の他Subnet CIDRへの暗黙allowを実効
  ルールへ追加する。実効ルールは新設の`NetworkInterfaceStatus.effective_ingress_
  rules`/`effective_egress_rules`（`spec`には混ぜない、`Create`/`Get`のみが返す）
  として表現し、VM Boot時の経路（`internal/compute/network.go`）・
  `UpdateFirewallRules`のNATS通知経路（`publishUpdateACL`）の両方をこちらに
  切り替えた。`kyuusha netif create/set-firewall-rules`の表示に`effective_
  ingress_rules`/`effective_egress_rules`を追加、`kyuusha subnet create`に
  `-shared-with-tenant-ids`を追加（それまでCLIから設定する手段が無かった）。
  `internal/network`にユニットテスト7件追加、全て通過を確認。

  実機検証で見つけたバグ: `internal/compute-agent/nftacl`（デフォルトSNAP
  実装）の`writeRule`が、`protocol`が`tcp`/`udp`/`icmp`のいずれでもない場合
  （`EffectiveFirewallRules`がmesh_group由来ルールに使う`protocol: ""`＝
  「プロトコル問わず」を含む）に**ルールを無言で捨てていた**——ユニットテストは
  ルール生成ロジックしか見ておらず、実際にnftablesへ反映されるかは
  playgroundで2つのSubnetを同じ`mesh_group`にして実VM間の実効ルールを
  `nft list ruleset`で確認して初めて発覚。`writeRule`に`protocol: ""`＝
  任意プロトコル（CIDRのみでマッチ）の分岐を追加して修正。同じ理由で
  `examples/snap-plugins/ebpf-snap`の`ruleFromFirewallRule`も
  `protocol: ""`を未知のプロトコルとしてエラーにしていたため、`protocol=0`
  （BPF側は元々`0`を「任意」として扱う設計だった）として扱うよう修正。
  `internal/compute-agent/nftacl`にこの分岐を検証するテストを追加。

  playgroundで実機確認済み: 同一テナント・同じ`mesh_group`の2 Subnetにそれぞれ
  VMを立て、明示ルール無しで`effective_ingress_rules`/`effective_egress_rules`
  に相手Subnet CIDRへの暗黙allowが現れ、実際に稼働Hypervisorのnftablesへ
  （両チェーンとも）反映されることを確認。他テナントの`shared_with_tenant_ids`
  未設定Subnet CIDRへの`allow`ルールを含む`UpdateFirewallRules`が`ErrValidation`
  で拒否され、`shared_with_tenant_ids`に自テナントを追加した後は同じ呼び出しが
  成功し、実効ルール・nftables両方に反映されることも確認。

- Image/Subnet/NetworkInterfaceにQuota適用を追加（`docs/architecture.md`
  「未決事項」節が残していた項目: `QuotaSpec`はvcpu/memory/volume_gb/vms/
  pci_devicesまでカバーしていたが、Image数・Subnet数・NetworkInterface数には
  上限が無く、1テナントが無制限に作成できた）。`QuotaSpec`に`max_images`/
  `max_subnets`/`max_network_interfaces`を追加し、`internal/image/quota.go`・
  `internal/network/quota.go`を新設（`internal/block-storage/quota.go`と
  同型の単一/複数次元OPA判定、`quota.rego`埋め込み）。image/networkの
  `Service`にそれぞれ`usageMu`+in-memory`usage`マップと、コンストラクタでの
  `rebuildUsage`（既存Image/Subnet/NetworkInterfaceの全件走査）を追加し、
  再起動時に使用量を失わないようにした（2026-09-13に見つかった同種のバグ
  クラスへの対策を横展開）。image/network/network-reconcilerの3バイナリに
  `-identity-addr`フラグとidentityへのmTLSクライアント配線を新規追加
  （これまでどちらもidentityへの依存が一切無かった）。`kyuusha tenant
  create/update`に`-max-images`/`-max-subnets`/`-max-network-interfaces`
  フラグを追加。実装の過程で、identity自身の`internal/identity/grpcserver`
  にある`QuotaSpec`のproto⇔ドメイン型変換（`fromQuota`/`toQuota`）が
  新フィールドを一切コピーしていなかったバグを発見・修正——プロトと
  各サービスのquota強制ロジックだけ更新して、identity自体の変換層の
  対応漏れに気づかないまま進めてしまっていた（変換層に既存のユニット
  テストが無かったことも一因）。再発防止に`internal/identity/grpcserver/
  server_test.go`を新設し、`QuotaSpec`の全フィールドを1つずつ突き合わせる
  ラウンドトリップテストを追加した。playgroundで実機確認済み:
  `-max-images=2 -max-subnets=1 -max-network-interfaces=1`のテナントに対し、
  Image/Subnet/NetworkInterfaceそれぞれ上限到達後の3件目Createが実際の
  gRPC経路（api-gateway→image/network）で`ResourceExhausted`相当の
  `InvalidArgument`として拒否されることを確認（詳細は
  [Quota仕様](specs/quota.md)「imageのQuota判定」「networkのQuota判定」参照）

- NetworkInterfaceのACL(`ingress_rules`、新規`egress_rules`)強制を実装し、
  セキュリティバックエンドをVNAPと同じ発想でプラガブルにした。`FirewallRule`
  メッセージ自体は既存のまま、`NetworkInterfaceSpec`に`egress_rules`
  （field 4）を新設。専用RPC`UpdateFirewallRules`（`resource_version`無し、
  Resizeと同じGet-then-mutate-then-Update規約）を追加し、Create後の
  ingress_rules/egress_rules変更にも対応——既存の汎用`Update` RPCはこの2
  フィールドの変更を拒否するよう変更し、変更経路を一本化した。
  compute-agent側に新規`-security-backend-bin`フラグ（`internal/
  compute-agent/secacl`が契約、既定実装は新規`internal/compute-agent/
  nftacl`）を追加。`network`独自のNATS JetStreamストリーム`NETWORK_CMD`
  を新設し、`UpdateFirewallRules`が対象VMの稼働Hypervisorへベストエフォート
  で変更を通知する（`cmd/network`がこの通知のためだけにcompute/NATSへ
  依存するようになった——`cmd/compute`が既に持つ同種の例外と同じ扱い）。

  実機検証で設計を1回転させている: 当初netdevファミリ（tapごとの独立した
  ingress/egressフック）で実装したが、`ct state established,related`が
  netdevファミリで使えない（"Protocol error"、conntrackが確立される前段の
  フックであるための構造的な制約）ことが判明し、bridgeファミリの
  forwardフック＋共有base chainへのjump方式に設計を変更した（`nft -j list
  chain`のJSON出力でjumpルールの既存有無・handleを判定し、重複追加/
  削除漏れを防ぐ）。この変更に伴い、デフォルト実装はtapがLinuxブリッジの
  ポートであることを前提とする——非ブリッジVNAP配線（EVPN Type-5等）では
  別のセキュリティバックエンドプラグインが必要になる、という制約を受け入れた
  （詳細は[network仕様](specs/network.md)「セキュリティバックエンド」、
  経緯は`docs/architecture.md`「ACL強制もVNAPと同じ発想でプラガブルに
  すべきか」参照）。`shared_with_tenant_ids`によるクロステナントCIDR検証と
  `mesh_group`自動許可は、今回は意図的にスコープ外のまま
  （[open-questions.md](open-questions.md)参照）。

  `kyuusha netif create`/新設`kyuusha netif set-firewall-rules`に
  `-ingress-rules`/`-egress-rules`（`protocol:port_range:source_cidr:action`の
  カンマ区切り）を追加。実装の過程で、`internal/gateway`の
  `NetworkInterfaceProxy`（api-gatewayがnetworkへ委譲する層）に
  `UpdateFirewallRules`の転送が無く、CLIから呼ぶと`Unimplemented`になる
  抜けを発見・修正——`internal/network/grpcserver`側にRPCハンドラを足しても、
  api-gateway側の委譲層は自動的には追従しないため、新RPCを追加する際は
  両方を見る必要がある、という教訓
  （`internal/gateway/networkinterface_proxy.go`）。

  playgroundで実機確認済み: `scenario.sh`のVM+NetworkInterface作成経路
  （`-subnets=`による実Firecracker起動）が新しいsecacl/nftacl配線を経ても
  無退行であること（実ゲストが起動しSubnetのgateway_ipへの実pingに成功）、
  対象tapに`kyuusha_acl`テーブル・`<tap>-in`/`<tap>-out`両チェーンが
  デフォルト拒否ベースライン（`ct state established,related`＋自Subnet CIDR＋
  gateway_ip許可＋末尾drop）付きで実際に作られること、`netif
  set-firewall-rules`呼び出し後に対象Hypervisorの`nft list chain`へ
  該当ルール（`tcp dport 22`/`tcp dport 443`等）が実際に反映されることを
  確認。なお「compute-agentプロセス再起動を跨いでnftables状態が残る」こと
  自体は`docker compose restart`では検証できなかった（Dockerコンテナの
  再起動はネットワーク名前空間ごと作り直し、tap/nftablesを含むnetns内状態と
  コンテナ内の全プロセス——Firecracker/jailerの子プロセスも——を道連れにする。
  VNAP以来のtap永続化の前提そのものに付随するplayground特有の制約で、
  今回のACL機能固有の問題ではない。詳細は[network仕様](specs/network.md)
  「デフォルト実装: internal/compute-agent/nftacl」参照）。
  併せて`playground/scenario.sh`自身の2つの既存バグ（`Subnet`/
  `NetworkInterface`のCreate直後レスポンスが`phase=Ready`である前提の
  チェックが、実際は非同期割当のため常にPending——`subnet get`/`netif get`
  でポーリングするよう修正）を検証中に発見・修正した（本来は今回のACL機能とは
  無関係な、以前の非同期割当移行以来の潜在バグ）。VolumeAttachmentが
  Attachedへ到達しない別の既存問題を検証中に見つけたが、こちらはblock-storage
  側の話でACL機能とは無関係のため、この変更では対応していない
  （別途調査が必要）。

- セキュリティバックエンドプラグインのTC-BPF（eBPF）参考実装
  `examples/security-plugins/ebpf-secacl`を追加（`cilium/ebpf`使用、
  独立したGoモジュールとしてルートの`go.mod`/`go.sum`には影響しない、
  VNAPの`examples/vnap-plugins/`と同じ「アダプトして使う参考実装」という
  位置づけ）。`nftacl`が前提とするLinuxブリッジ配線を必要とせず、tapの
  clsact ingress/egress両フックへTCX（`cilium/ebpf/link.AttachTCX`、
  qdisc不要な新しいカーネルAPI）で直接アタッチするため、非ブリッジVNAP配線
  （EVPN Type-5等）でも使える。netfilterのconntrackが無いTC-BPF向けに、
  正規化5-tupleキーの`LRU_HASH`マップ（全tap共有）で独自のステートフル
  実装（既存フローの自動許可）を実装した——ステートフル版として作り、
  性能重視のステートレス版は別途後日の予定。

  実装中に見つけた実機バグ: `cilium/ebpf`はBPFマップの値をホストのネイティブ
  バイトオーダーでシリアライズするため、Go側でCIDR/マスクを`binary.BigEndian`
  で組み立てるとリトルエンディアン環境で全アドレス比較が静かに壊れる
  （`binary.NativeEndian`が正しい）——veth実機テストで発見・修正、単体テスト
  だけでは気づけなかった類のバグ。

  実機確認済み（vethペア+network namespaceで実トラフィックを送って確認、
  `nft list ruleset`のテキスト確認だけだったnftacl検証より踏み込んだ検証）:
  自Subnet CIDR/gateway_ipへの疎通は常に許可、明示allowルール一致は通過、
  一致ルール無しの新規フローはdrop、明示denyルールでも新規フローはdrop、
  そして核心のステートフル性——`egress_rules`のみで許可されたVM発の
  フローについて、`ingress_rules`が空のままでも応答トラフィックが
  conntrack経由で通ることを確認。`detach`がTCアタッチメント・当該tapの
  pin済みマップを削除し（共有`conntrack`マップは残す）、2回呼んでも安全
  であることも確認。詳細は`examples/security-plugins/ebpf-secacl/README.md`
  参照。

- `.github/workflows/ci.yml`の`bufbuild/buf-setup-action@v1`に`github_token:
  ${{ github.token }}`を追加し、匿名GitHub APIレート制限の警告を解消した。

- `playground/scenario.sh`のVolumeAttachment検証バグを修正した（当日発見・
  当日修正。前段のCI整備時点では「別途調査が必要」としていたが、実際は
  block-storage側の実装ではなくこのスクリプト自身の潜在バグだった）:
  `volattach create`の応答を直接`phase=Attached`かどうかで判定していたが、
  `CreateVolumeAttachment`は常に`Pending`を返し、実際の排他制御チェック
  （`tryAttach`）は`cmd/block-storage-reconciler`が`Added`イベントに反応して
  非同期に行う（`internal/block-storage/service.go`のドキュメントコメント
  参照）。Subnet/NetworkInterfaceの「Create直後はPending、`get`でポーリング
  すべき」バグ（前回の`shared_with_tenant_ids`/`mesh_group`検証時に発見・
  修正済み）と同じクラスの問題で、reconcilerの反応が速いことが多いため
  たまたま通っていた不安定な検証だった。`wait_for_volume_ready`と同じ
  パターンの`wait_for_attachment_attached`ポーリングヘルパーを追加して
  修正、実機で`Pending`→`Attached`の遷移を確認した。

## 2026-09-26

- スケジューラにVolume容量ではなく**storage_connectionによるフィルタ**を追加
  （`docs/specs/volume.md`「スケジューリング時のフィルタリング」が長らく
  未実装として残していた項目）。VMが要求するVolumeの`storage_connection`を
  全て自己申告済みのHypervisorだけが候補に残る——`internal/compute/
  hypervisor_service.go`の`filterSchedulable`/`scheduleVM`/`scheduleMigration`
  を`scheduleConstraints`構造体（`Zone`/`StorageConnections`/`Exclude`）へ
  リファクタし、`validateVolumes`がzoneの導出と同じ仕組みで
  `storage_connection`一覧を返すように変更。Create時の初回スケジュール
  だけでなく、`Migrate`・`Resize`の容量不足フォールバックの再スケジュールも
  同じフィルタを通る。playgroundで実機確認済み: あるHypervisorだけに
  特定の`storage_connection`を宣言させ、空き容量では別のHypervisorが
  選ばれるはずの状況でも、正しくその接続を持つHypervisorへスケジュール
  され、Volumeが実際にAttachedまで到達することを確認
  （[VMスケジュール仕様](specs/vm-scheduling.md)「フィルタ（ハード制約）」参照）

- PCI/GPUパススルーのkyuusha側実装を追加（`docs/architecture.md`「PCIデバイス
  (GPU等)パススルー」節が長らく設計の型だけでTODOとしていた項目）。
  `RegisterHypervisorRequest.available_devices`フィールドを新設し、
  compute-agentの新しい`-pci-devices`フラグ（`pci_address:vendor_id:device_id`
  のカンマ区切り、宣言された各アドレスが実際に`vfio-pci`に束縛されているか
  `/sys/bus/pci/devices/<addr>/driver`で検証してから自己申告）で
  `Hypervisor.status.available_devices`に反映されるようにした。スケジューラの
  `filterSchedulable`/`scheduleVM`/`scheduleMigration`に`spec.pci_devices`
  （`vendor_id`/`device_id`/`count`）のフィルタと排他予約
  （`reservePciDevices`/`releasePciDevices`/`restorePciDevices`）を追加し、
  `chvmm`がcloud-hypervisor起動時に`--device path=/sys/bus/pci/devices/<addr>/,
  iommu=on`として反映するところまで配線した。テナント単位の統制として
  `Tenant.spec.quota.pci_devices`（`(vendor_id, device_id)`ごとの数量上限、
  リストに無い組は上限0の明示許可制）もCreate時のOPA判定に追加。
  `driver_hint`が`CLOUD_HYPERVISOR`以外のVMに`spec.pci_devices`を指定した
  場合は`validatePciDevicesForDriver`がCreate時に拒否する。
  **未検証**: このホストはBIOS/UEFI側でVT-d(IOMMU)が無効（DMARテーブル自体が
  存在しない）であることが判明し、物理的なBIOSアクセスが必要なため、実機での
  実際のVFIOパススルー動作は今回未確認——スケジューリング/予約/解放ロジックの
  ユニットテストとcloud-hypervisor起動引数の構築までを実装範囲とし、実機検証は
  BIOSでVT-dを有効化できる環境が整い次第の課題として残す
  （[VirtualMachine仕様](specs/virtual-machine.md)「PCIデバイスパススルー」、
  [Quota仕様](specs/quota.md)参照）

- 上記PCI/GPUパススルーの実機検証をBIOSでVT-dを有効化した上で実施し、成功を確認。
  ASMedia USB 3.1コントローラ（デスクトップ用途のGPU/NICより影響が小さい候補として選定）を
  `vfio-pci`へ再バインドし、`-pci-devices`自己申告→スケジューラの予約→`chvmm`の
  `--device`引数構築→実際に起動したcloud-hypervisorゲストのシリアルコンソールに
  そのデバイスの実PCI ID（`vendor_id:device_id`一致、USB/XHCIクラス）がPCIe Endpointとして
  そのまま見える、という経路を確認した。検証の過程で2つの結線バグを発見・修正:
  (1) `internal/compute/reconciler.go`の`CreateCommand`構築が
  `vm.status.allocated_pci_devices`を`PciDevices`フィールドへ一切詰めていなかった
  （スケジューラの予約自体は正しく動いていたが、compute-agentへは何も伝わっていなかった）、
  (2) `internal/identity`の`QuotaSpec`のGoドメイン型（`identity.QuotaSpec`）と
  gRPC変換（`fromQuota`/`toQuota`）が`pci_devices`フィールドを持っておらず、
  `Tenant.spec.quota.pci_devices`を設定してもidentity側で常に空に落ちていた
  （proto定義とcompute側の判定ロジックだけを見ていては気づけない、実際にCreate/Updateの
  往復をさせて初めて発覚したギャップ）。どちらも単体テストでは検出できなかった箇所——
  実際にVMを作成・起動させるplayground検証が無ければ「動くコードに見えて実際には
  何も配線されていない」状態のまま残っていた。あわせて`kyuusha tenant create`/
  `tenant update`に`-pci-device-quota`フラグを追加（それまでこのquotaを設定する
  CLI手段が存在しなかった）

- **NUMA/CPUピニング**（`spec.numa_pinned`）を実装。GPU/PCIパススルーの性能問題
  （デバイスと異なるNUMAノードのvCPUから触るとリモートメモリアクセスのレイテンシが
  乗る）がきっかけだったが、最終的にはPCIパススルーに限らない全VM共通の一般機能とし、
  ドライバも問わない形にした。compute-agentが`/sys/devices/system/node`から
  ホストのNUMAトポロジ（ノードID・所属CPU・メモリ量）を起動時に自動検出し
  `RegisterHypervisorRequest.numa_nodes`で自己申告、スケジューラ
  （`filterSchedulable`のフィルタ9種目、`reserveNumaNode`/`resizeNumaNodeCapacity`/
  `releaseNumaNode`/`restoreNumaNode`）がvCPU/メモリ・PCIデバイスと同じ
  Get→mutate→Updateパターンで単一ノードへの固定を予約・解放する。実装機構は
  cloud-hypervisor固有のCLIフラグ（`--numa`/`--memory-zone`/`--cpus affinity=`）では
  なく、`internal/compute-agent/cgroup`が既に両ドライバへ適用しているcpu.max/
  memory.maxの仕組みへ`cpuset.cpus`/`cpuset.mems`を足す形にした——libvirt/QEMUが
  既定で使うのと同じcgroup v2 cpusetアプローチで、Firecracker/cloud-hypervisor
  どちらのVMMプロセスにも同じ経路で効く。`kyuusha vm create -numa-pinned`/
  `hypervisor get`のnuma_nodes表示を追加。実機確認済み: このホスト自体はNUMAノード
  1個の構成だが、`spec.numa_pinned=true`のVMを両ドライバで作成し、実際に起動した
  プロセスのcgroup（`cpuset.cpus`/`cpuset.mems`）がホストの申告したノードと
  一致すること、Resizeでノードの`allocated_vcpu`/`allocated_memory_mb`が
  正しく増減すること、Deleteで解放されること、compute-agent再起動後もin-flightな
  予約が保持されることを確認した（[VirtualMachine仕様](specs/virtual-machine.md)
  「NUMA/CPUピニング」、[VMスケジュール仕様](specs/vm-scheduling.md)参照）

- **ルートディスク転送**（`Migrate(transfer_root_disk=true)`）を実装。
  `docs/open-questions.md`が長らく「未着手」としていた、コールドマイグレーション
  後にroot diskの中身（Imageからクローンした後にゲストが書いた差分）が
  失われる制限への対応。既定`false`のままなら従来通り（オプトイン、実ディスク
  サイズ相応のコスト増を伴うため）。転送経路はHypervisor間の新規データパスでは
  なく、既存のImage配布経路（OCIレジストリ）を再利用する形にした（ユーザー
  確認の上での決定）——`internal/compute-agent/imagestore`に`PushOCIBlob`/
  `DeleteOCIRef`を追加（`kyuusha image build`の既存push実装
  `orasPushFile`と同じ形）、`vmm.VMM`に`RootDiskPath`を追加してfcvmm/chvmmの
  現在のroot diskファイルパスを純粋関数として取得可能にし、
  `MigrateArtifactCommand`（PUSH/DELETE）という新しいNATSコマンドで
  旧HypervisorにpushさせてからmigrateVMが新Hypervisorのスケジューリングへ
  進み、新Hypervisorはこれを普通のImage取得と区別せず`EnsureCached`でpullする。
  失敗時はMigrate自体を`Unmigratable`/`RootDiskTransferFailed`で失敗させ、
  安い経路へ黙ってフォールバックしない。新VM起動確認後（成功・失敗いずれの
  `CreateResult`でも）にレジストリ上の一時アーティファクトを削除するfire-and-
  forgetクリーンアップも実装（ベストエフォート）。`kyuusha vm migrate
  -transfer-root-disk`、compute-agentの新しい`-migration-registry`/
  `-migration-registry-ref`/`-migration-registry-plain-http`フラグを追加。
  playgroundで実機確認済み: 実際にFirecracker VMを起動し、ホスト側のroot
  diskファイルへ直接書き込んだマーカーバイトが、転送後に移行先Hypervisorの
  新しいroot diskファイルの同じオフセットにそのまま存在すること、レジストリに
  一時アーティファクトが実際にpushされ新VM起動確認後に削除されること
  （`REGISTRY_STORAGE_DELETE_ENABLED=true`を設定したplayground用
  `registry:2`で確認）を確認した（[VirtualMachine仕様]
  (specs/virtual-machine.md)「ルートディスク転送」参照）

## 2026-09-25

- `VirtualMachineService.Create`向けのAdmission Webhook（Kubernetesの
  `ValidatingAdmissionWebhook`相当）を実装。新規`internal/admissionwebhook`
  パッケージ（リソース非依存、HTTP POST+JSON、gRPCではない）を`compute`サービス
  起動時の`-admission-webhook-urls`（カンマ区切り、複数指定可）で有効化。
  Image/NetworkInterface/Quotaの内部バリデーションを全て通した後・実際に
  永続化する前の最後のゲートとして呼ばれ、設定した全URLが`allowed:true`を
  返して初めて許可する（1つでも拒否すれば全体を拒否）。webhookが疎通不能な
  場合の挙動は`-admission-webhook-fail-open`で選択可能（既定fail-closed）。
  webhook URLの一覧はサービス起動時のオペレータ設定のみで、APIからテナントが
  登録する経路は作らない——`internal/compute-agent/netsetup`のVNAPプラグイン
  と同じセキュリティ上の割り切り。playgroundで実機確認済み（許可/拒否双方の
  応答、webhook疎通不能時のfail-closed拒否、拒否されたCreateがVMを一切
  永続化しないことを確認。[外部システム連携仕様](specs/external-integration.md)
  「ゲート系(作成側): Admission Webhook」参照）

- `docs/network-deployment-guide.md`に、EVPN Type-5（pure L3）デプロイ向けの
  「3.5. Type-5デプロイの場合」節を追加。`vlan_id`は厩舎自身のローカルな帳簿番号
  （Hypervisor上のブリッジ/ルーティング分離キー）であり、ワイヤ上の本物の802.1Qタグ
  であることを強制されない点、「1 VLAN = 1 VRF」という既定マッピングがType-5には
  適用されない点、Route Distinguisherを`vlan_id`単体から機械的に導出してはいけない点
  （AZ内でのみ一意なため）を明記。あわせて`examples/vnap-plugins/frr-type5.sh`
  （VNAPのサンプル実装）を追加——共有ブリッジを使わず、VMごとのtapへ`gateway_ip`を
  `/32`で直接付与しproxy ARPを有効化した上で、VM自身のIPを`/32`のホストルートとして
  カーネルとFRR（`vtysh`経由）へ注入する。playgroundで実機確認済み（[network仕様]
  (specs/network.md)「VNAP（ローカルなtap配線プラグイン契約）」参照）

- `internal/compute-agent/netsetup`に、tap配線のローカルなスイッチattach/detach
  ステップを外部バイナリへ委譲できるVNAP（VM Network Attach Protocol）契約を実装。
  compute-agentの`-network-attach-bin`で指定、未指定なら既存の固定Linuxブリッジ
  実装のまま。tapデバイス自体の作成・削除は常に厩舎が担い、プラグインは
  「作成済みのtapをローカルスイッチへattach/detachする」ことだけを担当する
  （CNI互換ではなく、バイナリ+stdin JSON+exit codeという呼び出し規約パターンだけを
  参考にしたkyuusha独自の契約——`docs/architecture.md`「VMのネットワーク接続を
  CNIのようにプラガブルにすべきか」参照）。playgroundで実機確認: 既定のLinux
  ブリッジ経路は無変更のまま実VM起動を確認、外部プラグイン経由でも正しいattach/
  detach JSONペイロードを受け取りゲストが正常に起動、プラグイン失敗時もtap自体は
  必ず削除されることを確認（[network仕様](specs/network.md)「VNAP（ローカルな
  tap配線プラグイン契約）」参照）

## 2026-09-24

- `VirtualMachineService.Resize`のコールド経路に、容量不足時のマイグレーション
  フォールバックを追加（`ResizeVirtualMachineRequest.allow_migrate`、既定
  `false`）。現在のHypervisorに新サイズが収まらない場合、`allow_migrate=true`を
  明示した時だけ`Migrate`と同じコールド移動を併用して収まる別Hypervisorへ
  移す（root diskはそのケースだけImageから作り直され中身が失われる——既定
  falseのままなら従来通りroot diskは無傷でResourceExhaustedのまま）。
  `Reconciler.ResizeWithMigration`として実装、`grpcserver.Server.Resize`が
  通常のコールドResizeが`ErrHypervisorCapacityExceeded`で失敗した場合のみ
  フォールバックとして呼ぶ。CLIは`kyuusha vm resize -allow-migrate`
  （[VirtualMachine仕様](specs/virtual-machine.md)「容量不足時のマイグレーション
  フォールバック」参照）

## 2026-09-23

- `VirtualMachineService.Migrate`を実装。`Stopped`のVMを別Hypervisorへ移す
  コールドマイグレーション（ライブ経路は無し、既存の「ライブマイグレーション不要」
  方針のまま）。root diskはImageから移行先で作り直され、NetworkInterface（IP/MAC）
  とVolumeAttachment（Volumeデータ）はHypervisor非依存の参照モデルのまま無傷で
  引き継がれる。新phase`Migrating`（`Stopped → Migrating → Scheduled`と合流し、
  以降は既存のCreate/Start経路をそのまま再利用）、`scheduleMigration`
  （現在のHypervisorを自動選択から除外、または`target_hypervisor`明示指定時は
  同じ既存フィルタで検証）を追加。playgroundで実機確認済み: 実行中VMをStop→Migrate
  （自動選択）で別Hypervisorへ移し、Firecrackerゲストが新Hypervisor上で起動、
  NetworkInterfaceのIP/MACが移行前と完全に同一であること、旧Hypervisor側の
  jail/runディレクトリが後始末されることを確認（[VirtualMachine仕様]
  (specs/virtual-machine.md)「マイグレーション」参照）

## 2026-09-22

- `internal/compute-agent/imagestore.Store`にキャッシュエビクション（LRU＋参照カウント除外
  ＋サイズ閾値、`docs/architecture.md`「イメージのローカル管理」で既に決定していた方針）を
  実装。cache-hitのたびにmtimeを更新し（最終アクセス順の実現）、実行中VMがカーネルを
  直接参照し続ける間は`Pin`/`Unpin`で除外対象にし、`Sweep`が古い順に削除して
  `-image-cache-max-mb`（既定20GiB、0で無効化）を超えないようにする。compute-agentの
  `-image-cache-sweep-interval`（既定10分）タイマーで定期実行。compute-agent再起動時に
  `Reconcile`が拾い直す既存VMも`vmm.BootRecord.PinnedKeys`経由で正しく再pinされる
  （[Image仕様](specs/image.md)「ローカルキャッシュのエビクション」参照）

## 2026-09-20

- `VirtualMachineService.Resize`/`AttachVolume`/`DetachVolume`にライブ経路を追加。
  `Running`+`driver_hint=CLOUD_HYPERVISOR`のVMに対し、cloud-hypervisorの
  `--api-socket`経由でダウンタイム無しのvcpu/memoryリサイズ・Volume着脱ができる
  ようになった（コールド経路と同じRPCでphase/driver分岐、`Stopped`のVMは従来通り
  コールド動作）。Firecrackerは構造的にホットプラグ不可能なため対象外。
  新規`internal/compute-agent/chapi`（cloud-hypervisor api-socketクライアント）・
  `internal/compute/liveops.go`（`Reconciler`側の実装）を追加
  （[VirtualMachine仕様](specs/virtual-machine.md)「リサイズ」「Volume
  attach/detach」、[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)
  「`--api-socket`」参照）

## 2026-09-19

- `VolumeAttachment`の命名スキームを位置ベース（`volattach-<vm-id>-<index>`）から
  VolumeIDベース（`volattach-<vm-id>-<volume-id>`）へ変更。`AttachVolume`/`DetachVolume`
  実装に伴い、途中要素の削除で後続indexがずれ既存attachmentが孤児化するバグを回避するため
  （[Volume仕様](specs/volume.md)「compute側の統合」参照）
- VM Resize（コールド、Stopped限定）、Volume attach/detach（コールド）を実装

## 2026-09-15

- `chvmm`にUEFIブート（edk2の`CLOUDHV.fd`）を追加し、`QCOW2`（ブートローダー内蔵の
  自己完結ディスク）経由の起動をサポート。`fcvmm`は構造的に非対応のまま
  （[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)「起動方式2: UEFIブート」参照）
- `kyuusha image build`を実装（`cmd/kyuusha/imagebuild.go`）。
  `docker build` → `docker export | tar -x` → `mkfs.ext4 -d` → `oras-go/v2`でOCIレジストリへ
  push → `ImageService.Create`という一気通貫パイプライン
  （[Image仕様](specs/image.md)参照）

## 2026-09-14

- イメージのローカル管理（Track 1）: fcvmm/chvmm共有の`internal/compute-agent/imagestore`
  を新設し、digest検証付きcontent-addressedキャッシュ・reflinkによるVM専用CoWコピーを実装。
  containerdの`content`/`snapshots`パッケージは依存が重すぎる・kyuushaの単一ディスクイメージ
  という要求と噛み合わないと判断し不採用、自前の小さなパッケージにした
  （[docs/architecture.md](architecture.md)「イメージのローカル管理」参照）
- イメージのOCIレジストリ対応（Track 2）: `oras-go/v2`採用、`ImageArtifact.url`に
  `oci://`スキームを追加

## 2026-09-13

- `network`/`block-storage`のreconcileループを、compute同様に単一インスタンスの
  別プロセス（`cmd/network-reconciler`/`cmd/block-storage-reconciler`）へ分離
- 認可ロールに`role=network-admin`（networkサービスにscopeしたadmin相当）・
  `role=viewer`（全テナント・全サービス横断read-only）を追加
- 払い出したリソース自身のメトリクス（VirtualMachine/NetworkInterface/Volume）を全種実装
- 孤児リソースGC（10分間隔の定期スイープ）をNetworkInterface/VolumeAttachmentへ適用
- バグ修正: `vm create -subnets=`がtap配線されないまま起動する不具合
  （NetworkInterfaceの非同期IP割り当てをcompute側が待たずbootへ進んでいたのが原因、
  `createNetworkInterfaces`に短時間ポーリングを追加して解消）

## 2026-09-12

- `VirtualMachineService.Stop`/`Start` RPCを実装
- pet/cattleの区別を廃止: `recovery_policy`/`persistent_root_disk`/`status.root_volume_ref`
  を削除（実質未使用だったフィールド。ハイパーバイザー喪失時の自動リカバリはKaaS層/
  オペレータに委ねる判断、[docs/architecture.md](architecture.md)「ハイパーバイザー死活監視と
  リカバリ、およびpet/cattleの区別の廃止」参照）
- VMMドライバをQEMU直接execからcloud-hypervisor直接execへ置き換え
- バグ修正: `Delete`後もjail/runディレクトリの実体が永久にリークし続けていた不具合
  （`handleDelete`が呼ぶメソッドを`Stop`から`Destroy`へ変更）

## 2026-09-11

- バッキングストアをオンメモリのmapからetcdへ移行。`internal/resource.Store`を
  etcd-backedに書き換え、5サービス全てが`-etcd-endpoints`経由で接続するよう変更
  （[docs/architecture.md](architecture.md)「コントロールプレーンサービス自体の可用性」参照）
- ハイパーバイザー向けの本格PKI（証明書動的発行）を不採用と確定。代わりにbootstrapトークンを
  軽量拡張し個体識別・失効を実現（将来の`Register`拒否のみ、既存セッションの強制切断は不可）
- 認可ロールに`tenant_role=viewer`（テナント内read-only）・`role=storage-admin`
  （block-storageサービスにscopeしたadmin相当）を追加
- block-storageに`StorageConnection`リソースを新設し、Volume作成時の到達性/実在性検証を
  非同期Pending→Readyパターンで実現
  （[docs/architecture.md](architecture.md)「『参照するだけ』の弱点をStorageConnection
  リソースで埋める」参照）

## 2026-09-10

- block-storageの責務境界を「プロビジョニング＋export」から「参照＋接続」へ縮小。
  専任サービス`storage-agent`（`StorageBackend`ドライバ抽象化でプロビジョニング・export・
  compute-agent側のiSCSI/NVMe-oFイニシエータ接続まで担っていた）を削除。
  「利用組織ごとに選びたいストレージバックエンドが全く違う」という現実を軽視した設計だった
  ため（[docs/architecture.md](architecture.md)「block-storageのバックエンド抽象化」参照）

## 2026-09（初版）

- 専用ストレージノード + iSCSI/NVMe-oF（ZFSバックエンド）方式でblock-storageを実装
  （後日2026-09-10に責務ごと削除、上記参照）
