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
