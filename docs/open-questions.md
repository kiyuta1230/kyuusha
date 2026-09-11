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

## Hypervisorのストレージ接続自己申告を動的化すべきか（静的で実装中、将来の検討事項）

`StorageConnection`実装（上記）は当面、Hypervisorの`storage_connections`自己申告を
起動時1回きりの静的な宣言として扱う。将来的に、heartbeatのたびに実際に
到達性を再確認してから報告する動的な方式に変える価値があるか、という
オープンクエスチョン。

- **静的（今の実装）**: シンプル。ただし接続が後から切れても検知できず、
  一度`Ready`になった`StorageConnection`/`Volume`の状態が陳腐化しうる
- **動的**: heartbeatごとに再確認するので状態を正直に保てる。ただしNFSが
  ハングした場合の`stat()`呼び出しが返ってこず、heartbeatループ全体を
  詰まらせるリスクがある（別goroutine+タイムアウトでの隔離が要る等、
  実装コストが上がる）

`StorageConnection`の実装完了後、運用実績を見てから判断する。この節は
`StorageConnection`関連の他の節と違い、実装が終わっても削除しない
（恒常的な将来課題として残す）。

## Volumeの申告内容（存在確認・サイズ）が一切検証されない（解決済み、下記参照）

もともとの問題: `CreateVolume`が`protocol`/`storage_connection`/`identifier`の
形式チェックのみで即座に`Ready`にしていたため、到達可能性も`size_gb`（完全な
自己申告、`max_volume_gb`のQuota強制が事実上の申告制になっていた）も一切検証
されなかった。2段階で解決済み:

1. **サイズの検知**（2026-09-11、commit `30409fb`）: VM起動時、
   `internal/compute-agent/volumeref.Resolve`が見つけたパスの実サイズを取得し、
   宣言`size_gb`と10%以上食い違えば`vmm.WarnIfSizeMismatch`が構造化ログで警告する
   （block-storage/proto側の変更は伴わない、ログベースの検知のみ）
2. **存在確認**（2026-09-11、`StorageConnection`リソース＋非同期検証フロー実装）:
   `Volume`はもう即`Ready`にならず、`StorageConnection`が`Ready`（宣言した全ゾーンで
   到達確認済み）かつ`identifier`の実在がcompute-agent経由で確認できて初めて
   `Ready`になる。詳細は`docs/architecture.md`「追記（2026-09-11）」と
   `docs/specs/volume.md`「検証フロー」参照

**まだ解決されないまま残る部分**（別タスク）:
- サイズ食い違いのAPI越しの可視化は解決済み（2026-09-11）: `VerifyVolumeResult`の
  `size_bytes`（元々存在していたのに捨てられていた）を`handleVerifyResult`で
  `spec.size_gb`と比較し、`Volume.status.conditions`に`SizeMatchesDeclaration`
  （True/False）として記録するようにした。`kyuusha volume get`が`conditions=...`
  を表示するようになったので、そこで見える（不一致でも`Ready`自体はブロックしない
  ——警告であって存在確認の失敗ではないため）。`vmm.WarnIfSizeMismatch`（VM起動時の
  ログ警告）はそのまま残す——こちらはVolume検証と独立にVM起動のたびに実際の
  アタッチ経路で発生するので、両方に意味がある
- `device_path`/`status.hypervisor`（`VolumeAttachment`側）のAPI越しの可視化は
  まだ未解決——`VolumeAttachmentStatus`に`DevicePath`/`Hypervisor`フィールド自体は
  あるが、compute-agentからの報告経路がまだ無い
- VM起動が寛容に劣化しない件（`volumeref.Resolve`が失敗するとVM起動全体が失敗する）
  自体は変更していない——永続データを積むVolumeを黙って外すより失敗を明示する方が
  安全という判断で、"バグ"ではなくトレードオフとして残している
- Volume検証コマンドのリトライ間隔・上限（現状は無期限、10秒ごと）は未チューニング

## QEMU用のjailer相当の隔離方式（2トラックに分割決定、下記参照）

2026-09に`driver_hint=FIRECRACKER`のVMをjailer（chroot + uid/gid権限降格）でラップした
（[Firecracker起動仕様](specs/firecracker-boot.md)「jailer」、docs/architecture.md
「Firecracker: jailerとtapデバイス」参照）。jailer自体はFirecracker専用ツールで
QEMUをラップできないため、`driver_hint=QEMU`にはまだ同等の隔離が無いまま。

libvirt経由でQEMUを使う案は見送り（kyuusha全体の「運用コストの重い既製品を避ける」
路線とズレる、コンテナ環境でのnamespace関連の深いバグを既に何度も踏んでいる実績から
libvirt+SELinux/AppArmorでも同種の沼にハマるリスクが高い）。自前実装の方向で確定——
ただしchroot/uid-drop（低リスク、`fcvmm/jailer.go`のパターンを流用できる）と
namespace分離+seccomp（高リスク、標準ライブラリに無くlibseccompかBPF自前実装が要る）
の難易度差が大きいため、2026-09-11に以下へ分割することを決定:

1. **kyuusha本体**: chroot + uid/gid dropのみの軽い版（Firecracker側のjailerと
   同じスコープ）。**意図的に保留中**（2026-09-11、ユーザー判断）——別プロジェクト
   （2番目）が思ったより早く仕上がる可能性があり、その場合この軽い版自体が
   無駄になる。別プロジェクトの進み具合を見てから着手するかどうかを決める
2. **別プロジェクト（新規に立ち上げ、別セッションで着手予定）**: namespace分離+
   seccompまで含む本格的な「QEMU用jailer」を、Firecracker用`jailer`と同じ立ち位置
   （汎用的な既製ツール）で育てる

詳しい経緯・検討した選択肢・新プロジェクトが目指すべき機能・kyuusha側の参考実装は
[QEMU jailerハンドオフ](qemu-jailer-handoff.md)に切り出した——別プロジェクトを
立ち上げる新しいセッションは、まずそちらを読むこと。

## ロールベースの細かい認可（RPCメソッド・リソース種別単位）をやるべきか

現状`role`クレームは実質`admin`かそれ以外かの2値でしか使われておらず、「このロールは
CreateはできるがDeleteはできない」のようなメソッド単位・リソース種別単位の制御はない
（[認証・認可仕様](specs/authn-authz.md)参照）。加えて、`internal/authz`のinterceptorは
そもそもOPAへRPCメソッド名を渡していない（`tenant_id`/`role`のみ）ため、ポリシーを
書き足すだけでは足りずinterceptor自体の拡張も要る。

docs/architecture.mdでは「主要な外部クライアントはKaaSコントローラー1つで、自クラスタの
全リソース種別を管理する必要があるため分割の実利が薄い」として意図的に据え置いていた
（OPA採用によりいつでも先送りできる、という前提込みで）。判断保留中。
