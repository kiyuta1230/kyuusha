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
- **NUMA/hugepages/CPU topology**: より柔軟。SELF_HEALのpet的ワークロード
  (長時間稼働・性能重視のVM)向け
- **Windowsゲスト対応の可能性**: cloud-hypervisorはUEFI/OVMF経由のブートにも
  対応しており、将来非Linuxゲストが必要になった際の選択肢になりうる
  (現状の直接カーネルブート方式では引き続き非対応、下記参照)

これらはいずれもVMM自体の機能差であり、下記の起動方式(kernel直接ブート)を
選んだこととは独立——起動方式を変えても得られる/得られないわけではない。

### 起動方式: カーネル直接ブート(ブート可能ディスクではない)

cloud-hypervisorは`--kernel`にPVHエントリポイントを持つvmlinuxを渡す形の
直接カーネルブートをネイティブにサポートしている(BIOS/ブートローダーを経由
しない、Firecrackerと同じ思想)。このためFirecracker用に既にある
`KERNEL_ROOTFS`Imageをそのまま流用でき、新しいビルド時アセットが不要——
QEMU時代のような「本来の起動方式(QCOW2)をあえて選ばない」という妥協は
そもそも発生しない。

**トレードオフ(意図的に受け入れている制約)**: 現状の実装は依然として
**Linux専用**——BIOS/UEFIファームウェアを経由しないため、Windowsのような
非Linuxゲストは起動できない。将来必要になったら、cloud-hypervisorのUEFI/OVMF
ブートパスを使う別実装(まだ存在しないパス)を足す判断になる——
`docs/open-questions.md`参照。

## computeからcompute-agentへ渡る情報

ドライバを問わず共通の`CreateCommand`ペイロード(`driver_hint`が
`CLOUD_HYPERVISOR`になっているだけ)——[VirtualMachine仕様](virtual-machine.md)
「computeからcompute-agentへ渡る情報」参照。

## compute-agent側の起動処理(`internal/compute-agent/chvmm`)

1. **キャッシュ確認・rootfsの複製**: fcvmmと同じ手順(別ディレクトリ: 既定
   `/var/lib/kyuusha/ch-cache`・`/var/lib/kyuusha/ch-run`。同じ資産でも
   ドライバごとに別々にキャッシュ・コピーする——ディスク上の状態をドライバ単位で
   独立して調べられるようにするためで、資産自体がFirecracker用/
   cloud-hypervisor用で違うわけではない)
2. **起動**: 以下のcloud-hypervisor引数を組み立てて実行する

   | 引数 | 値/意図 |
   |---|---|
   | `--kernel <kernel_path>` `--cmdline "<boot_args>"` | 直接カーネルブート。
     Firecracker用の`defaultBootArgs`とほぼ同じだが、Firecrackerの
     `is_root_device`自動注入相当が無いため`root=/dev/vda rw`を明示する
     (下記参照) |
   | `--cpus boot=<vcpu>` `--memory size=<memory_mb>M` | `spec.vcpu`/
     `spec.memory_mb`をそのまま |
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

- 上記「起動方式」で述べた通り、非Linuxゲスト(Windows等)は現状サポート外
  ——UEFI/OVMFブートパス経由の別実装が要る
- `QCOW2`形式のImageはまだどのドライバにも消費されない(`internal/compute`の
  Reconcilerが`spec.disk`を読んでいない。[Image仕様](image.md)参照)——将来の
  ブート可能ディスクサポートが実装されて初めて意味を持つ
- PCI passthrough (VFIO)・vhost-user networking: 上記「なぜcloud-hypervisorも
  要るのか」で挙げた本来の動機そのものは、まだどちらも未実装
- cloud-hypervisor APIソケット(`--api-socket`)経由の制御(ライブマイグレーション、
  ホットプラグ等): 使っていない(起動時にCLIフラグを一括で渡すだけ)
- 外部jailerによるchroot/namespace/uid-gid drop: 静的バイナリ+組み込み
  seccompで足りると判断し、意図的に導入していない(上記「QEMUからの置き換え」
  参照)。Landlockによるファイルシステムアクセス制限の追加も同様に未着手

ドライバを問わず共通の未実装事項(digest検証、`user_data`の機密情報対応、
本物のcloud-initでの動作確認)は
[VirtualMachine仕様](virtual-machine.md)「この実装がカバーしないもの(共通)」
参照。
