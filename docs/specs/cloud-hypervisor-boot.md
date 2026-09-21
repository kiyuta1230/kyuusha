# cloud-hypervisor起動仕様

## 概要

`driver_hint=CLOUD_HYPERVISOR`のVirtualMachineは、compute-agentが実際に
cloud-hypervisorプロセスを起動する。[Firecracker起動仕様](firecracker-boot.md)と
並ぶ2つめの実VMM統合で、起動する`Image`の中身(`KERNEL_ROOTFS`: カーネル+生rootfs、
ブートローダーなし)はFirecracker側と完全に同一——同じImageを
`driver_hint=firecracker`と`driver_hint=cloud-hypervisor`のどちらでも起動できる
([Image仕様](image.md)参照)。実装は`internal/compute-agent/chvmm`。

### 2026-09-12: QEMUからcloud-hypervisorへの置き換え

このドライバは元々`qemu-system-x86_64`を直接execする実装(QEMU)だった。QEMU用の
外部jailer(chroot+uid/gid drop)を設計している最中、実際のバイナリを`ldd`で
確認したところ**約30個の共有ライブラリ**(libglib、libgnutls、libslirp等)に
動的リンクされていること、加えて直接カーネルブートでも内部的にBIOS/VGA-BIOS等の
レガシーPCファームウェア資産(`/usr/share/qemu/`配下)を必要とすることが判明した。
jailer(AWS製、Firecracker用)と違い、minijail0はexec対象バイナリ自体すら
jail内へコピーしてくれないため、これら全てを毎回chroot内に用意する必要があり、
Firecracker側の「jailer.goのパターンをほぼそのまま流用できる、低コスト」という
前提が崩れた。

この時点で「そもそもQEMUである必要があるのか」を再検討し、
[cloud-hypervisor](https://www.cloudhypervisor.org/)(Firecrackerと同じ
`rust-vmm`系統、Intel/ARM/Alibaba等が関わるプロジェクト)へ置き換えることにした:

- **静的バイナリ配布あり**(`cloud-hypervisor-static`)——`ldd`で"statically
  linked"と出る。共有ライブラリのchroot問題がそもそも発生しない
- BIOS/VGA-BIOS等のレガシーファームウェア資産が不要(direct kernel bootが
  正式サポート、回避策ではない)
- **seccompをバイナリ自体に内蔵**(既定で有効)、Landlockもオプションで内蔵——
  外部jailerが実質不要になった(Firecracker側のjailerのような別プロセスは無し)
- QEMUを選んでいた本来の理由(VFIO passthrough、vhost-user)は両方
  cloud-hypervisorもネイティブにサポートしている
- 起動方式がCLIフラグ駆動(`--kernel --cmdline --disk --net --cpus --memory`)
  で、kyuushaの既存の「プロセス1個起動して追跡する」という設計パターンに
  そのまま乗る

**確認できたトレードオフ**: VFIOパススルーはcloud-hypervisor側がQEMUより
未成熟(GPU/IOMMU/PCIセグメント周りのバグ報告あり)。ただしkyuusha自体、
VFIO/vhost-userはどちらもまだ未実装(下記「この実装がカバーしないもの」参照)
なので、切り替え時点での実害はない。

置き換え前に、このコンテナ環境で`cloud-hypervisor-static`が既存の
kernel+rootfsキャッシュ資産(fcvmm用に既にダウンロード済みのもの)を無変更で
実際に起動できることをフィージビリティ検証済み——`kyuusha: guest booted OK`
まで到達することを確認した。

### なぜFirecrackerがあるのにcloud-hypervisorも要るのか

Firecrackerは意図的に最小限のデバイスモデルしか持たない(PCIバス自体が無い)。
cloud-hypervisorを選ぶ理由はそこにある:

- **PCI passthrough (VFIO)**: GPU等のパススルーはPCIバスを持つ側でのみ成立する。
  まだ未実装だが、将来足す際の受け皿になる
- **vhost-user networking**: Firecrackerはtapのみでvhost-userに対応しない。
  OVS-DPDKのような高スループット経路を使うには前提になる
- **NUMA/hugepages/CPU topology**: より柔軟。長時間稼働・性能重視のワークロード向け
- **Windowsゲスト対応の可能性**: cloud-hypervisorはUEFI/OVMF経由のブートにも
  対応しており、QCOW2形式のImageを介して実装済み（下記「起動方式: QCOW2
  ブート」参照。UEFI機構そのものはAlpine公式cloud imageの実機ブートで実証済みだが、
  Windows自体での確認はまだ）

これらはいずれもVMM自体の機能差であり、下記の起動方式(kernel直接ブート)を
選んだこととは独立——起動方式を変えても得られる/得られないわけではない。

### 起動方式1: カーネル直接ブート(`KERNEL_ROOTFS`)

cloud-hypervisorは`--kernel`にPVHエントリポイントを持つvmlinuxを渡す形の
直接カーネルブートをネイティブにサポートしている(BIOS/ブートローダーを経由
しない、Firecrackerと同じ思想)。このためFirecracker用に既にある
`KERNEL_ROOTFS`Imageをそのまま流用でき、新しいビルド時アセットが不要——
QEMU時代のような「本来の起動方式(QCOW2)をあえて選ばない」という妥協は
そもそも発生しない。

この起動方式は依然として**Linux専用**——BIOS/UEFIファームウェアを経由しない
ため、Windowsのような非Linuxゲストは起動できない。非Linuxゲストが要る場合は
下記の起動方式2(`QCOW2`)を使う。

### 起動方式2: UEFIブート(`QCOW2`、2026-09-15実装)

`Image.spec.format=QCOW2`の場合、`internal/compute-agent/chvmm`は
`--kernel`/`--cmdline`の代わりに`--firmware`でedk2ベースのUEFIファームウェア
（`CLOUDHV.fd`）を渡し、`--disk`に`spec.disk`のqcow2ファイルをそのまま
（`image_type=qcow2`、rawへの変換はしない——下記参照）渡す。ゲスト自身の
ブートローダー（GRUB等）・カーネルがその後を引き継ぐ、実機のUEFIマシンと
同じ流れ。

- **ファームウェアはフルUEFI版（`CLOUDHV.fd`）を採用**、軽量な
  Rust Hypervisor Firmwareは使わない——QCOW2対応の動機がWindows等の
  非Linuxゲストであり、そのためには完全なUEFI/ACPI実装が要るため。
  `docker/Dockerfile`のcompute-agentステージが
  `https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd`
  から、firecracker/jailer/cloud-hypervisor本体と同じ「GitHub releasesの
  既製バイナリをそのまま取ってくる」パターンで取得する（既定配置先
  `/usr/local/share/kyuusha/CLOUDHV.fd`、`-ch-firmware-path`で変更可）
- **qcow2はそのまま渡す、rawへ変換しない**: cloud-hypervisor公式のQuick
  Startガイドは`qemu-img convert`でraw変換してから渡す例を示しているが、
  `--disk`の`image_type=qcow2`パラメータ自体は正式にドキュメント化された
  サポート済み機能。変換をしない判断の理由: (1) `qemu-img`という新規依存が
  compute-agentに増えない、(2) 起動のたびに変換する時間的コストが無い、
  (3) qcow2は圧縮/sparse形式なのでraw展開すると使用容量が増える
- `spec.boot_args`はこの起動方式では使わない（UEFI起動はゲスト自身の
  ブートローダーがカーネルコマンドラインを決めるため、cloud-hypervisorの
  `--cmdline`はそもそも渡さない）。ネットワーク設定は
  `kyuusha.net.<i>.*`カーネルパラメータ慣習ではなく、cloud-init NoCloud
  seed disk（`spec.user_data`）経由で行う想定——実機のcloud imageは
  こちらを読む
- キャッシュ・VM専用コピーの仕組み（digest検証・content-addressed
  キャッシュ・reflink CoW）はTrack 1/2で作った`internal/compute-agent/
  imagestore`をそのまま再利用——ファイルの中身がkernel/rootfsかqcow2かを
  区別しない設計だったため、変更不要だった
- **fcvmm（Firecracker）はこの起動方式に一切関与しない**——構造的にUEFIを
  経由できないため。`internal/compute/image.go`のCreate時バリデーションが
  `QCOW2`形式に`driver_hint=CLOUD_HYPERVISOR`を強制しており、fcvmmへ
  QCOW2のVMが渡ることはない

**実機確認（2026-09-15）**: kyuusha自身のテストアセットではなく、**Alpine
Linux公式のcloud image**（`generic_alpine-3.22.5-x86_64-uefi-tiny-r0.qcow2`、
無改変）をそのままOCIレジストリへpushし、`format=qcow2`のImageから実際に
VM起動。コンソールでedk2のBDS（Boot Device Selection）ログ→GRUBメニュー→
Linuxカーネル起動→`vda2`（qcow2内の実パーティション）からのrootfsマウント→
`Welcome to Alpine Linux 3.22`のログインプロンプト到達まで確認した。

## computeからcompute-agentへ渡る情報

ドライバを問わず共通の`CreateCommand`ペイロード(`driver_hint`が
`CLOUD_HYPERVISOR`になっているだけ)——[VirtualMachine仕様](virtual-machine.md)
「computeからcompute-agentへ渡る情報」参照。

## compute-agent側の起動処理(`internal/compute-agent/chvmm`)

1. **キャッシュ確認・複製**: fcvmmと同じ`internal/compute-agent/imagestore.Store`
   （既定`/var/lib/kyuusha/image-cache`）をfcvmmと**共有**する——同じImageをFirecracker用/
   cloud-hypervisor用で別々にダウンロード・保持することはない（[Firecracker起動仕様]
   (firecracker-boot.md)「compute-agent側の起動処理」参照）。VM専用の書き込み可能な
   コピー（`<ch-run-dir>/<vm_id>/rootfs.raw`、またはQCOW2なら`disk.qcow2`）は
   reflink（`FICLONE`）が使えるファイルシステムならcopy-on-writeで複製し、
   使えなければ通常コピーへフォールバックする
2. **起動**: `spec.disk`（QCOW2）の有無で分岐し、以下のcloud-hypervisor引数を
   組み立てて実行する

   | 引数 | 値/意図 |
   |---|---|
   | `--kernel <kernel_path>` `--cmdline "<boot_args>"` | 直接カーネルブート。
     Firecracker用の`defaultBootArgs`とほぼ同じだが、Firecrackerの
     `is_root_device`自動注入相当が無いため`root=/dev/vda rw`を明示する
     (下記参照) |
   | `--cpus boot=<vcpu>,max=<...>` `--memory size=<memory_mb>M,hotplug_size=<...>` | `spec.vcpu`/
     `spec.memory_mb`が起点。`max=`/`hotplug_size=`は全CLOUD_HYPERVISOR VMに
     無条件で付与するライブリサイズ用の余地（下記「`--api-socket`」参照） |
   | `--api-socket <path>` | 同じくライブホットプラグ用（下記参照）。全CLOUD_HYPERVISOR
     VMに無条件で付与 |
   | `--disk path=...` | rootディスク(`rootfs.raw`、virtio-blk、`/dev/vda`)。
     seed diskがあれば2枚目を`readonly=on`で追加、Volumeがあれば3枚目以降 |
   | `--net tap=<name>,mac=<addr>` | `internal/compute-agent/netsetup`が実際に
     作ったtapデバイスをそのまま使う(fcvmmと全く同じtap/ブリッジ配線。tap自体は
     どのVMMプロセスが繋がっても同じ)。IPアドレス自体はkyuusha側の慣習
     (`kyuusha.net.<i>.*`カーネルパラメータ)でゲスト自身が設定するため、
     `ip=`/`mask=`パラメータは渡さない |
   | `--serial tty` `--console off` | ttyS0のみがI/O面。cloud-hypervisor
     自身のstdout/stderrを`console.log`へ丸ごとリダイレクトすることで、
     ゲストのシリアル出力と起動時エラーの両方を1ファイルで見られるようにする
     (fcvmmの`console.log`契約と同じ考え方) |

   `--seccomp`は明示的に渡していない(既定で`true`)。外部jailerは使っていない
   ——静的バイナリで共有ライブラリが無く、組み込みseccompで足りると判断した
   (上記「QEMUからの置き換え」参照)。

3. **成否判定**: fcvmm/chvmm共通の500ms猶予期間方式
   ([VirtualMachine仕様](virtual-machine.md)「起動確認(成否判定)」参照)。
   cloud-hypervisor固有の即時失敗(バイナリが無い、`/dev/kvm`権限がない、
   カーネルイメージがPVHエントリポイントを持たない、等)を検知するためのもの

### boot_argsのデフォルトがFirecrackerと少し違う理由

FirecrackerはドライブAPIの`is_root_device`フラグを見て、ゲストのカーネル
cmdlineへ`root=/dev/vda`相当を**自動的に**注入する(ユーザーが明示しなくて
よい)。cloud-hypervisorにはこの自動注入が無いため、chvmm独自のデフォルト
`boot_args`は`root=/dev/vda rw`を明示的に含む:

```
console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/init
```

同じ`KERNEL_ROOTFS`Imageを両ドライバで使い回せる、という前提は保たれている:
`spec.boot_args`を明示指定しない限り、それぞれのドライバが自分に必要な形へ
自動的に補ってくれる。

## `--api-socket`（ライブホットプラグ、`Running`+`CLOUD_HYPERVISOR`限定）

全CLOUD_HYPERVISOR VMは起動時に無条件で`--api-socket <ch-run-dir>/<vm_id>/api.sock`
を渡される（`ConsoleLogPath`と同じ、`vm_id`の純粋関数——`runningVM`に新しい
フィールドは要らない）。`VirtualMachineService.Resize`/`AttachVolume`/`DetachVolume`
が`Running`+`CLOUD_HYPERVISOR`のVMに対して呼ばれたときのライブ経路
（[VirtualMachine仕様](virtual-machine.md)「リサイズ」「Volume attach/detach」参照）
専用で、それ以外の用途では一切参照しない。

**`internal/compute-agent/chapi`**（新規、標準ライブラリのみ）がこのソケットへ
HTTP(Unixドメインソケット経由)でPUTする、使用する3エンドポイントのみ:

| エンドポイント | 用途 | リクエストボディ |
|---|---|---|
| `PUT vm.resize` | vcpu/memory変更 | `{desired_vcpus, desired_ram(bytes)}` |
| `PUT vm.add-disk` | ディスクhotplug | `{path, id}`（`id`は呼び出し元指定——
  応答の`PciDeviceInfo`ではなく、常にVolumeAttachmentの`Meta.ID`をそのまま渡す） |
| `PUT vm.remove-device` | ディスク取り外し | `{id}`（`add-disk`に渡したのと同じid） |

`internal/compute-agent/chvmm`が`vmm.Hotplugger`インターフェース
（`LiveResize`/`LiveAddDisk`/`LiveRemoveDevice`）としてこれを実装する。
`fcvmm`は実装しない（Firecrackerは構造的にホットプラグ不可能、
[Firecracker起動仕様](firecracker-boot.md)「spec.vcpuの制約」参照）——
compute-agent側は`a.Drivers[driver_hint]`を`vmm.Hotplugger`へ型アサートして
判定するだけで、非対応ドライバに空実装を足す必要はない。

**`max=`/`hotplug_size=`のポリシー**: boot時vcpu/memoryの**2倍**（絶対上限
32vCPU/256GiB）に固定している（`internal/compute-agent/chvmm`の
`hotplugMaxVCPU`/`hotplugMemoryCeilingMB`）。「どうせ無条件で付与するなら
大きいほど良い」わけではない——cloud-hypervisorのACPI hotplugは実RAMこそ
実際にhot-addするまで消費しないが、ゲストのGPA/E820レンジとKVMメモリスロット
（virtio-blk/net等のデバイスBARと共有する有限リソース）は起動時点で確保する。
このため倍率は運用実績を見て見直す前提の暫定値。

**ライブ操作のNATS往復**（`internal/compute/liveops.go`）: `COMPUTE_CMD`
（`StopCommand`と同じat-least-once/ack-on-receipt）へ`HotplugCommand`を送り、
結果はper-request reply-subject（`StreamConsole`の`ConsoleRequest`と同じ
パターン、`COMPUTE_EVT`は使わない——このRPCを待っているのは呼び出し元1人
だけなので永続化不要）で同期的に受け取る。往復は10秒でタイムアウトし
`Unavailable`。

## UserData注入・cgroupリソース制限・Stop/Start・削除・シリアルコンソール

すべて[VirtualMachine仕様](virtual-machine.md)の該当節と同じ仕組みを共有する
(`internal/compute-agent/vmm`のBuildSeedDisk、`internal/compute-agent/cgroup`、
`Stop`のSIGTERM→3秒待ちSIGKILL/`force`即SIGKILL、`Destroy`のjail/runディレクトリ
削除、`kyuusha vm console`)。chvmm固有の差分はない。

## playgroundでの構成

`docker/Dockerfile`の`compute-agent`ステージで`cloud-hypervisor-static`を
GitHub releasesから直接フェッチしている(Firecracker本体と同じ「既製バイナリを
そのまま取ってくる」パターン)——kernel/rootfsアセット自体はFirecracker用に
既にある`image-assets`(vmlinux + Alpine minirootfsのext4)をそのまま使う。
`/dev/kvm`・tap配線・`privileged: true`・`-drivers`フラグ(playgroundは
`-drivers=FIRECRACKER,CLOUD_HYPERVISOR`——これが無いと`driver_hint=
CLOUD_HYPERVISOR`のVMはスケジュール不能になる)等、ドライバを問わず共通の要件は
[VirtualMachine仕様](virtual-machine.md)「playgroundでの共通構成」参照。
`playground/scenario.sh`は、Firecracker用のImageに対して
`-driver-hint=cloud-hypervisor`で追加のVMを1台作り、同じ`kyuusha vm console`
確認(`docker/fc-guest-init.sh`が出す起動メッセージ。ドライバによらず同じ文言
——ゲスト自身はどちらのVMMで起動されたか区別できないため)で実ブートを確認する。

## この実装がカバーしないもの

- Windows自体での実機確認はまだ（UEFI起動機構自体はAlpine公式cloud imageで
  実証済み——上記「起動方式2: UEFIブート」参照）
- PCI passthrough (VFIO)・vhost-user networking: 上記「なぜcloud-hypervisorも
  要るのか」で挙げた本来の動機そのものは、まだどちらも未実装
- `--api-socket`経由のライブマイグレーションは未実装（vcpu/memory resize・
  Volume attach/detachのホットプラグは実装済み、上記「`--api-socket`」参照）
- 外部jailerによるchroot/namespace/uid-gid drop: 静的バイナリ+組み込み
  seccompで足りると判断し、意図的に導入していない(上記「QEMUからの置き換え」
  参照)。Landlockによるファイルシステムアクセス制限の追加も同様に未着手

ドライバを問わず共通の未実装事項(`user_data`の機密情報対応、
本物のcloud-initでの動作確認)は
[VirtualMachine仕様](virtual-machine.md)「この実装がカバーしないもの(共通)」
参照(digest検証は実装済み——[Firecracker起動仕様](firecracker-boot.md)
「compute-agent側の起動処理」参照)。
