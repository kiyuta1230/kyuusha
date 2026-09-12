# QEMU用jailer相当ツール: 別プロジェクト立ち上げ用ハンドオフ

> **注（2026-09-12）**: このドキュメントが提案する「別プロジェクトとして立ち上げる」
> 方針は2026-09-12に見直され、撤回された。既製の汎用exec型jailer（minijail/nsjail）
> が実在することが分かったため、別プロジェクトは作らず、kyuusha本体
> （`internal/compute-agent/qemuvmm`）へ直接統合する方針に変更している。
> 現在の設計は[QEMU jailer設計](specs/qemu-jailer.md)を参照。本文は当時の検討過程の
> 記録として残す。

このドキュメントは、**kyuushaとは別の新しいプロジェクトとして**「QEMU用のjailer相当の
隔離ツール」を立ち上げるための引き継ぎ資料。2026-09-11、kyuushaの実装セッション内で
議論・決定した内容をまとめたもの——このドキュメント自体はkyuushaリポジトリに残すが、
実際の実装は別リポジトリで行う想定。新しいセッション（このリポジトリの文脈を持たない）が
これだけ読めば経緯と設計判断を再構築できることを目指す。

## そもそもの背景: kyuushaとFirecracker用jailer

kyuusha（厩舎、Go製の小規模IaaS/KaaS基盤）は、VMを2つのVMM（Virtual Machine Monitor）で
起動できる: Firecracker（`driver_hint=FIRECRACKER`、既定）とQEMU（`driver_hint=QEMU`）。
両方とも同じ`KERNEL_ROOTFS`形式のImage（カーネル+生rootfsイメージ、ブートローダー無し）を、
`internal/compute-agent`配下の別々のドライバ（`fcvmm`/`qemuvmm`）で起動する。

**Firecracker側は2026-09に実チェイジャー（`jailer`）でラップした**（AWS製、Firecracker
本体とは別の既製バイナリ、GitHubのFirecrackerリリースに同梱されている）:

- chroot（`--chroot-base-dir`で指定したディレクトリ配下に`<execファイルのbasename>/<vm_id>/root`
  を作り、そこにFirecracker自身をコピーして実行）
- uid/gid権限降格（`--uid`/`--gid`、全VM共通の固定値——VMごとに異なるuid/gidを割り当てる
  ことは意図的にスコープ外のまま）
- Firecracker自身が内蔵しているseccompフィルタ（jailerとは別だが、`--no-seccomp`を
  渡さない限りデフォルトで有効）

**意図的にやっていないこと**（Firecracker側もスコープ外のまま）: PID/network/mount
namespace分離、VMごとに一意なuid/gid。

kyuusha側の対応するコード（`internal/compute-agent/fcvmm/jailer.go`、実装は完了済み）:

```go
func resolveExecPath(bin string) (string, error)
// 「firecracker」のようなbareな名前を$PATH解決して絶対パスにする
// （jailerの--exec-fileは$PATH解決をしないため）

func jailChrootDir(chrootBaseDir, fcExecPath, vmID string) string
// jailer自身が作る/使うパスと同じ規則で
// <chrootBaseDir>/<execファイルのbasename>/<vmID>/root を計算する
// （jailerを起動する*前*にこの中へリソースを配置する必要があるため）

func placeReadOnlyResource(src, dst string) error
// 複数VMで共有される読み取り専用リソース（カーネルイメージ）をchroot内へコピー
// （chownせずworld-readableにする——共有キャッシュファイルの所有権を書き換えない）

func placeWritableResource(src, dst string, jailUID, jailGID uint32) error
// VM専用の書き込み可能リソース（rootfsのコピー）を配置してjailUID:jailGIDにchown

func mknodDeviceLike(src, dst string, jailUID, jailGID uint32) error
// 実ブロックデバイス（iSCSI/NVMe-oF経由のVolume）と同じmajor:minorの
// デバイスファイルをchroot内にmknodし、jailUID:jailGIDにchown
// （ハードリンクは別ファイルシステム間なので使えないため）

func placeVolumeLike(src, dst string, jailUID, jailGID uint32) (mounted bool, err error)
// mknodDeviceLike（ブロックデバイス）とbind mount（NFS等の通常ファイル）を
// 使い分ける統合版。bind mountはchownしない——同じinodeなので、chownすると
// NFSサーバ上の実ファイルまで変わってしまうため
```

呼び出し側（`internal/compute-agent/fcvmm/manager.go`のBoot()）が、これらを使って
chroot内へkernel/rootfs/seed disk/Volumeを配置してから、実際に`jailer`バイナリを
`exec.Command`で起動する（`--exec-file`にFirecracker本体、`--uid`/`--gid`/
`--chroot-base-dir`を渡す）。

## QEMU側の現状（ギャップ）

`internal/compute-agent/qemuvmm/manager.go`は`qemu-system-x86_64`を直接execしている
——chrootもuid/gid降格もseccompも無し。compute-agentコンテナ自体が特権コンテナで
root実行なので、QEMUプロセスもroot・無隔離のまま動いている。これが埋めたいギャップ。

## 検討した選択肢とその評価

### 選択肢1: libvirt経由でQEMUを使う（見送り）

OpenStack Novaも採用している現実的な方式。SELinux/AppArmorによる自動閉じ込め
（svirt）、非root実行、PCI/VFIOパススルーの成熟したサポートを一括で得られる。

**見送った理由**:
- libvirtd自体が新しい重量級の依存になり、kyuusha全体の「Goから`exec.Command`で
  直接VMMを起動する」というシンプルな設計から離れる（tap配線・console.log・QMP
  周りもlibvirtのdomain modelへ作り直しになる）
- kyuushaプロジェクト全体の一貫した判断（Ceph不採用、Kafkaの代わりに軽量NATS選択等、
  「運用コストの重い既製品を避ける」路線）とズレる
- kyuushaの実装セッションだけでも、ZFSのmount namespace問題・iSCSIのnetwork
  namespace問題等、コンテナ環境特有の深いバグを複数回踏んでいる。libvirt+
  SELinux/AppArmorをこの手の環境に入れると同種の沼にハマるリスクが高い

ユーザーコメント: 「そこまでクリティカルには感じていない」——完全な却下ではなく、
優先度が低いという位置付け。

### 選択肢2: 自前でchroot/namespace/uid-drop相当を実装する（採用）

Goの`syscall.SysProcAttr`（`Chroot`/`Cloneflags`/`Credential`）で、`fork+exec`の
境界にカーネルへ直接処理させる経路がある。3つのパーツに分けて評価:

| パーツ | 難易度 | 補足 |
|---|---|---|
| chroot + uid/gid drop | **低** | `fcvmm/jailer.go`のパターンをほぼそのまま流用できる |
| PID/mount namespace分離 | **中** | 後述「未解決の設計課題」参照 |
| seccomp | **高** | 後述参照。一番リスクが高い部分 |

**PID/mount namespace分離の未解決課題**: 現状、tapデバイス（VMのネットワークI/F）は
compute-agentコンテナ自身のネットワーク名前空間に作られている
（`internal/compute-agent/netsetup`）。QEMU側に独自のnetwork namespaceを与えるなら、
tapをその中へ移すか、veth pairを新たに作ってbridgeするかの設計判断が必要——
まだ決めていない。mount namespaceも、procfs/devfsの再マウント等、地味に詰める点が残る。

**seccompが一番重い理由**: Goの標準ライブラリにseccompサポートが無い。libseccompの
cgoバインディングを使うか、自前でBPF（`unix.SockFilter`）を組むしかない。フィルタが
緩すぎれば意味がなく、厳しすぎるとQEMU自体が想定外のsyscallで落ちる——
Firecracker側は実際にAWS製の`jailer`にそのまま乗ることでこのリスクを丸ごと避けられたが、
QEMU向けには同等の既製品が無い（QEMUのデバイスモデルはFirecrackerよりずっと大きく
可変なので、そもそも「これだけ許可すればよい」というsyscallセットが固定しにくい）。

## 最終決定（2026-09-11）: 2トラックに分割する

1. **kyuusha本体（近い将来、別セッションで着手）**: 軽い版——chroot + uid/gid drop
   のみ実装する。`fcvmm/jailer.go`のパターンを`qemuvmm`向けに書き直すだけなので、
   実装コストは低い。namespace分離・seccompは入れない（Firecracker側のjailerと
   同じスコープに揃える）。**注: このドキュメント作成時点でまだ未着手**
2. **別プロジェクト（今回のハンドオフ対象）**: 本格的なnamespace分離+seccompまで
   含む、汎用的な「QEMU用jailer」を独立プロジェクトとして育てる——Firecracker用
   `jailer`と同じ立ち位置（kyuushaに限らず誰でもexecして使える、汎用的な既製ツール）
   を目指す。理由:
   - kyuusha自身のロードマップを、一番リスクの高い部分でブロックしなくて済む
   - kyuushaが既に「jailerは外部の既製バイナリに任せる」という構造を持っているので、
     QEMU側も同じ構造に揃える方が一貫性がある
   - QEMU向けにはこの立ち位置の既製品が今のところ無く、reusableな価値がありそう
     （kyuusha以外からの需要も見込める）

**トレードオフとして認識していること**: 別プロジェクトにしても、namespace/seccomp
自体の実装難易度は減らない——どこにコードを置くか・どうリリースするかという
組織上の判断でしかない。かつ、別リポジトリ・別ビルド/リリースパイプライン・
バージョニングという運用コストが新たに乗る（kyuusha側のDockerfileも、今の
Firecracker/jailerと同じ「GitHub releasesから既製バイナリを取ってくる」パターンに
変える必要が出てくる、そのプロジェクトが実際にリリースを持つようになった時点で）。

## 新プロジェクトが目指すべき機能（Firecracker用jailerとのパリティ+α）

`fcvmm/jailer.go`の実装を参考実装として踏襲しつつ、以下をカバーする想定:

- 実行ファイルパスの解決（bareな名前を$PATH解決）
- chrootディレクトリの作成/再利用（`<execファイルのbasename>/<インスタンスid>/root`
  のような規則）
- 読み取り専用の共有リソース（カーネルイメージ等、複数インスタンスで共有）を
  chown無しでコピー配置
- 書き込み可能な専用リソース（rootfsコピー等）をjailのuid/gidへchownして配置
- ブロックデバイス（実iSCSI/NVMe-oF LUN等）を同じmajor:minorでchroot内へmknod
- 通常ファイル（NFS等でマウント済みの永続ボリューム）をbind mountで配置
  （**コピーしない**——ゲストの書き込みが実バッキングストアへ届かなくなり、
  永続化の意味が壊れるため。bind mountはchownしない——同じinodeなので、
  chownすると元ファイルの所有権まで変わってしまう）
- uid/gid権限降格
- （Firecracker用jailerには無い、新規）PID/mount/network namespace分離
- （Firecracker用jailerには無い、新規）QEMUの実際のsyscallフットプリントに
  合わせたseccompフィルタ

## kyuusha側の参考コード・ドキュメント（新プロジェクト設計時に読むべきもの）

- `internal/compute-agent/fcvmm/manager.go` — Boot()内でjailer.goの各関数を
  どういう順序で呼んでいるか（chroot作成→リソース配置→jailer実行）の実例
- `internal/compute-agent/fcvmm/jailer.go` — 上記の低レベル配置ロジックそのもの
- `internal/compute-agent/qemuvmm/manager.go` — 現状のQEMU起動処理（隔離なし。
  「軽い版」を足す先であり、新プロジェクトが最終的に統合される先でもある）
- `internal/compute-agent/netsetup` — tapデバイス配線（namespace分離時に
  設計判断が要る箇所）
- `docs/specs/firecracker-boot.md`「jailer」節
- `docs/specs/qemu-boot.md`

## kyuusha固有の前提（新プロジェクト側では気にしなくてよいこと）

- kyuusha自身のストレージ責務境界（Volumeはプロビジョニングせず参照のみ、
  `docs/architecture.md`「訂正: 責務の境界を...」参照）は、このツール自体の設計には
  無関係——「ブロックデバイスとファイルの両方を配置できる」という要求だけ理解して
  いればよい
- Firecracker用jailerも「全VM共通の固定uid/gid」というスコープ縮小をしている
  （VMごとに一意なuid/gidは意図的に非対応）。新プロジェクトもこのスコープに
  揃えるかどうかは自由——広げても構わない
