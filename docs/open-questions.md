# 実装フェーズの迷い事項

docs/architecture.md の「未決事項」は設計レベルの論点用。こちらは実装を進める中で
出てきた、まだ判断を保留している細かい話を随時追加していくメモ。

## CLIの `-tenant` フラグをトークンのクレームからデフォルトすべきか

`kyuusha vm`/`kyuusha tenant` の各サブコマンドは `-tenant`/`-id` を常に明示指定させている。
非adminにとっては認可上どうせ自分自身の `tenant_id` としか一致しないため実質冗長だが、
adminは「どのテナントを操作するか」を明示する必要があり、トークンのクレームから
自動導出する設計にはできない（意味が壊れる）。

やるとしたら: CLI側だけの利便性機能として、`-tenant` 省略時はローカルのトークン
（署名検証はせず、単にpayloadをデコードするだけ）から `tenant_id` クレームを読んで
デフォルト値にする、という変更。ワイヤプロトコル・authzモデルには一切影響しない。

判断保留中。理由: 地味に便利だが、CLIの挙動が「明示的な引数」から「暗黙のデフォルト」に
変わることそのものが本当に良いか自信がない（未指定時に「どのトークンのクレームが
使われたか」が見えにくくなる懸念もある）。

## Hypervisor自己登録の認証（解決済み: zone検証のみ実装、証明書発行は保留）

2026-09に、東西通信全体のmTLS化（`internal/mtls`）と、`RegisterHypervisor`の
zoneスコープ付きbootstrapトークン検証（`internal/bootstraptoken`）を実装した。
ただし元の設計の「identityが軽量CAを兼ねてハイパーバイザー専用のmTLS証明書を動的発行する」
部分は意図的にスコープ外とした——identityが秘密鍵を持ち、発行・run時保存・失効化まで
面倒を見る本格的なPKI運用になり、今回の「まず軸を絞って安全に着手できる範囲」という
判断とは規模が違いすぎると判断したため。詳細は
[Hypervisor登録・死活監視仕様](specs/hypervisor-bootstrap.md)「既知の未実装事項」参照。

残っているのは主に: (1) ハイパーバイザー単位の識別・失効（現状は「zoneスコープの
トークンを持っている」ことしか確認できない）、(2) bootstrapトークンの使い捨て化。
どちらも必要になった時点で着手する、という判断で今は先送りしている。

## QEMUドライバのブート可能ディスク対応（Windows等の非Linuxゲスト）

2026-09に`driver_hint=QEMU`を実装した（`internal/compute-agent/qemuvmm`）。ただし起動方式は
Firecrackerと同じ「kernel/rootfsを直接指定する」方式を選び、QEMUが本来可能な「ブートローダー
内蔵の自己完結ディスク」（`QCOW2`）経由の起動は実装しなかった——同じ`KERNEL_ROOTFS`資産を
両ドライバで使い回せることを優先したため（[QEMU起動仕様](specs/qemu-boot.md)「起動方式」参照）。

この選択の対価として、Windows等の非Linuxゲストは現状サポート外（BIOS/UEFIファームウェアを
経由しないため原理的に起動できない）。`Image.spec.format=QCOW2`自体はスキーマ・
Create時バリデーションとも既に存在するが、どのドライバもまだ`spec.disk`を消費しない
（Reconcilerが読んでいない）——本当に必要になった時点で、QCOW2を実際に起動する新しい
パスを`qemuvmm`に足す（chroot/ファームウェア起動を含む、既存のkernel/rootfs直接ブートとは
別の実装になる見込み）という判断で今は先送りしている。

## block-storageバックエンドの責務境界の作り直し（解決済み・実装済み、2026-09-10）

2026-09-10、`docs/architecture.md`「block-storageのバックエンド抽象化」節を、
「厩舎がプロビジョニング＋export＋接続まで全部やる」という当初の`StorageBackend`
設計から、「プロビジョニングとHypervisor単位の接続確立（iSCSI/NVMe-oFログイン、
NFSマウント）は運用者側の責務、厩舎は参照メタデータ＋排他制御＋スケジューリング
制約＋起動時のデバイス/ファイル発見のみを持つ」という縮小した境界へ書き換え、
実装した（commit `92d359b`。`storage-agent`・`internal/compute-agent/iscsi`は
完全に削除、新しい`internal/compute-agent/volumeref`が発見ロジックのみ担う）。
iSCSI/NVMe-oF/NFSは「Hypervisor単位の事前接続＋接続済みセッション内の1リソースを
参照するだけ」という統一モデルに収まった。`playground/scenario.sh`で実VM経由の
永続化を実際に確認済み（詳細は`docs/specs/volume.md`参照）。

## Hypervisor↔ストレージバックエンドの接続確立をkyuusha側で自動化すべきか（未着手、下記参照）

上記の実装直後、ユーザーから: 「登録されたボリューム接続情報に従って、
ハイパーバイザが自動でマウントなりしておくことはできるよね？」という指摘。
現状（上記で実装した設計）は、Hypervisor自身が`-storage-connections=
name[:local_path]`フラグで「もう接続済み」と自己申告する方式——新しい
ストレージバックエンドを1つ追加するたびに、そのゾーンの既存Hypervisor**全台**の
フラグを書き換えて再起動する必要があり、運用コストがHypervisor数×ストレージ
バックエンド数のオーダーで増え続ける。

**実例との比較で分かったこと**: OpenStack Cinder（nova-compute側のos-brick）も
Kubernetes CSIも、「プロビジョニング」（ボリュームの作成/削除、バックエンド固有）は
外部化・プラガブルにする一方、「ホスト接続確立」（iSCSIログイン、NFSマウントに
相当する`NodeStageVolume`）はオーケストレータ自身が自動でやる——kyuushaが
直前に決めた「接続確立も含めて完全に外部化する」という境界は、これらの実例より
一歩ドライブ過ぎている可能性がある。プロトコル標準の接続確立をkyuusha側へ
戻すのは、直前の設計の後退ではなく、実例に近づける精緻化と捉えられる。

**頻度分析（ユーザーと確認済み）**: 自動化する場合、Hypervisor側の作業は
「ホスト初期セットアップ時（iscsiadm/nvme-cliインストール、認証情報配置）」の
1回きりになり、その後は:
- 新しいストレージバックエンドが増えたとき → そのゾーンの各Hypervisorが
  自動で初回接続する（人手不要になる）
- 既に接続済みのバックエンドの中でVolumeが増えるだけ → 接続作業は一切不要
  （NFS/NVMe-oFは即座に見える。iSCSIだけ`iscsiadm --rescan`相当の軽い自動
  再スキャンが要るかもしれないが、`volumeref.Resolve`内に隠蔽でき、人間の
  作業は発生しない）

**大まかな形（ユーザー提案、まだ詳細未確定）**: block-storageサービスに
新しい`StorageBackend`（または`StorageConnection`）リソースを作り、実接続情報
（iSCSIならポータルIP:port+IQN、NVMe-oFならdiscoveryアドレス+NQN、NFSなら
サーバー:ベースパス、どのゾーンから到達可能か）を持たせる。ストレージ管理者が
Subnetと同じ粒度（1ストレージノードにつき1回）で登録する。compute-agentがそれを
Watchし、自分のゾーンに該当するものへ自動接続する。

**まだ決まっていないこと（ユーザーの意向で一旦保留、着手しない）**:
- 接続確立のタイミング: eager（Hypervisor起動時に自ゾーンの全StorageBackendへ
  先回り接続）か、lazy（実際にそのVolumeを使うVMがスケジュールされた瞬間だけ
  接続）か
- リソースの正式な形・名前（`StorageBackend`か`StorageConnection`か、
  独立したCRUDリソースかそれ以外か）
- スケジューリング時、Volumeのstorage_connectionを持たないゾーンのHypervisorを
  除外するフィルタ（自動化する場合はほぼ必須になる——手動declare方式では
  「起動はするが劣化する」で済んでいた部分）
- `kyuusha volume create`のCLI verb見直し（ユーザー提案、まだ「かもね」レベル）
  ——RPC名`Create`自体はKubernetesの`PersistentVolume`も同じ「参照のみ・
  それでもcreateと呼ぶ」という前例があり、他リソースとの一貫性のため維持が
  妥当そうだが、Cinderの`volume create`（実プロビジョニングする）との紛らわしさ
  を避けるCLIエイリアス（`register`等）は検討の余地がある

この節は設計討議の記録であって実装計画ではない——次に着手するときはまず
上記の未決事項から詰める。

## QEMU用のjailer相当の隔離方式（自前実装 vs. libvirt）

2026-09に`driver_hint=FIRECRACKER`のVMをjailer（chroot + uid/gid権限降格）でラップした
（[Firecracker起動仕様](specs/firecracker-boot.md)「jailer」、docs/architecture.md
「Firecracker: jailerとtapデバイス」参照）。jailer自体はFirecracker専用ツールで
QEMUをラップできないため、`driver_hint=QEMU`にはまだ同等の隔離が無いまま。

検討した選択肢:

1. **libvirt経由でQEMUを使う**: OpenStack Novaも採用している現実的な方式。SELinux/
   AppArmorによる自動閉じ込め（svirt）、非root実行、PCI/VFIOパススルーの成熟した
   サポートを一括で得られる。ただし: (a) libvirtd自体が新しい重量級の依存になり、
   今の「Goから`exec.Command`で直接VMMを起動する」というシンプルな設計から離れる
   （tap配線・console.log・QMP周りもlibvirtのdomain modelへ作り直しになる）、
   (b) このプロジェクト全体の「運用コストの重い既製品を避ける」判断（Ceph不採用、
   Kafkaの代わりに軽量NATS選択等）と路線がズレる、(c) このセッションだけでも
   ZFSのmount namespace問題・iSCSIのnetwork namespace問題等、コンテナ環境特有の
   深いバグを何度も踏んでおり、libvirt+SELinux/AppArmorをこの環境に入れると
   同種の沼にハマるリスクが十分ある
2. **自前でchroot/namespace/uid-drop相当を実装する**: Goの`syscall.SysProcAttr`
   （`Chroot`/`Cloneflags`/`Credential`）で`fork+exec`境界にカーネルへ直接処理させる
   経路があり、比較的安全に実装できる。ただしseccompフィルタは標準ライブラリに無く、
   自前でBPFを組む/libseccompバインディングを使うしかない——ここは緩すぎても
   厳しすぎても事故る、一番リスクの高い部分。Firecracker側は実際にAWS製の
   `jailer`にそのまま乗ることでこのリスクを避けられたが、QEMU向けの同等の
   既製品は無い

ユーザーの反応: 自前実装の方向に傾いているが未確定。libvirt依存については
「そこまでクリティカルには感じていない」とのコメントあり——完全に却下したわけ
ではなく、判断を先送りしている状態。もし自前実装を選ぶ場合は、chroot/namespace/
uid-drop部分（低リスク）とseccomp部分（高リスク、最初は入れない/様子見という
選択肢もある）を分けて考える方針で一致している。

## ロールベースの細かい認可（RPCメソッド・リソース種別単位）をやるべきか

現状`role`クレームは実質`admin`かそれ以外かの2値でしか使われておらず、「このロールは
CreateはできるがDeleteはできない」のようなメソッド単位・リソース種別単位の制御はない
（[認証・認可仕様](specs/authn-authz.md)参照）。加えて、`internal/authz`のinterceptorは
そもそもOPAへRPCメソッド名を渡していない（`tenant_id`/`role`のみ）ため、ポリシーを
書き足すだけでは足りずinterceptor自体の拡張も要る。

docs/architecture.mdでは「主要な外部クライアントはKaaSコントローラー1つで、自クラスタの
全リソース種別を管理する必要があるため分割の実利が薄い」として意図的に据え置いていた
（OPA採用によりいつでも先送りできる、という前提込みで）。判断保留中。
