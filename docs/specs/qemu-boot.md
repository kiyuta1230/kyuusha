# QEMU起動仕様

## 概要

`driver_hint=QEMU`のVirtualMachineは、compute-agentが実際にQEMU（`qemu-system-x86_64`）
プロセスを起動する。[Firecracker起動仕様](firecracker-boot.md)と並ぶ2つめの実VMM統合で、
起動する`Image`の中身（`KERNEL_ROOTFS`: カーネル+生rootfs、ブートローダーなし）はFirecracker側と
完全に同一——同じImageを`driver_hint=firecracker`と`driver_hint=qemu`のどちらでも起動できる
（[Image仕様](image.md)参照）。実装は`internal/compute-agent/qemuvmm`。

### なぜFirecrackerがあるのにQEMUも要るのか

Firecrackerは意図的に最小限のデバイスモデルしか持たない（PCIバス自体が無い）。QEMUを
選ぶ理由はそこにある:

- **PCI passthrough (VFIO)**: GPU等のパススルーはPCIバスを持つQEMU/libvirt側でのみ
  成立する（`docs/architecture.md`「認可の粒度」及び「PCI/GPUデバイス」節参照）。まだ
  未実装だが、下記の通りQEMU側の機械種別選択（`q35`）はこれを見越している
- **vhost-user networking**: Firecrackerはtapのみでvhost-userに対応しない。OVS-DPDKの
  ような高スループット経路を使うにはQEMUが前提になる（`docs/architecture.md`の未決事項）
- **NUMA/hugepages/CPU topology**: QEMUの方が柔軟。SELF_HEALのpet的ワークロード
  （長時間稼働・性能重視のVM）向け
- **virtio-blk実装差**: Firecrackerの素朴な実装がetcd的な同期fsyncワークロードに不利な
  可能性（要ベンチマーク、`docs/architecture.md`参照）。QEMU側が有利な可能性がある

これらはいずれもQEMUという**VMM自体の機能差**であり、下記の起動方式（kernel/initrd直接
ブート）を選んだこととは独立——起動方式を変えても得られる/得られないわけではない。

### 起動方式: kernel/initrd直接ブート（ブート可能ディスクではない）

QEMUは本来、ブートローダー内蔵の自己完結ディスクイメージ（`QCOW2`、`docs/architecture.md`
「QCOW2」節参照）を素直に起動できるVMMだが、この実装は**あえてそちらを選ばず**、
Firecrackerと同じ「カーネル+rootfsをkyuusha側が直接指定する」方式
（QEMUの`-kernel`/`-append`、Linux直接ブートプロトコル）を採用している。理由:

- 同じ`KERNEL_ROOTFS`Imageをそのまま流用でき、新しいビルド時アセット（本物のブート可能
  qcow2イメージ）を用意する必要がない
- 上記のQEMUを選ぶ本来の動機（PCI passthrough/vhost-user/NUMA等）はVMMの機能差であって
  起動方式とは無関係なので、この選択でも失われない

**トレードオフ（意図的に受け入れている制約）**: この方式は**Linux専用**——ブートローダー/
ファームウェア（BIOS/UEFI）を一切経由しないため、Windowsのような非Linuxゲストは原理的に
起動できない。Secure Boot/TPM測定起動/BitLockerのようなファームウェア依存機能も同様に
使えない。将来Windows等が本当に必要になったら、ブート可能ディスク経由の別実装（QCOW2を
実際に消費する、まだ存在しないパス）を足す判断になる——`docs/open-questions.md`参照。

## computeからcompute-agentへ渡る情報

ドライバを問わず共通の`CreateCommand`ペイロード（`driver_hint`が`QEMU`になっているだけ）
——[VirtualMachine仕様](virtual-machine.md)「computeからcompute-agentへ渡る情報」参照。

## compute-agent側の起動処理（`internal/compute-agent/qemuvmm`）

1. **キャッシュ確認・rootfsの複製**: fcvmmと同じ手順（別ディレクトリ: 既定
   `/var/lib/kyuusha/qemu-cache`・`/var/lib/kyuusha/qemu-run`。同じ資産でもドライバごとに
   別々にキャッシュ・コピーする——ディスク上の状態をドライバ単位で独立して調べられるように
   するためで、資産自体がFirecracker用QEMU用で違うわけではない）
2. **起動**: 以下の`qemu-system-x86_64`引数を組み立てて実行する

   | 引数 | 値/意図 |
   |---|---|
   | `-M q35` | PCIeバスを持つ機械種別。**あえて`microvm`（Firecracker類似の最小構成）を
     選ばない**——`microvm`にはPCIバス自体が無く、将来のVFIO passthroughを閉ざしてしまう
     ため（上記「なぜQEMUも要るのか」参照） |
   | `-enable-kvm` | KVMアクセラレーション必須（`/dev/kvm`が要る。fcvmmと同じ前提） |
   | `-cpu host` | ホストCPUの機能をそのまま見せる |
   | `-smp <vcpu>` / `-m <memory_mb>M` | `spec.vcpu`/`spec.memory_mb`をそのままQEMUの
     仮想トポロジへ |
   | `-kernel <kernel_path>` `-append "<boot_args>"` | 直接カーネルブート。Firecracker用の
     `defaultBootArgs`とは異なる独自のデフォルトを持つ（下記参照） |
   | `-drive file=...,format=raw,if=virtio` | rootディスク（`rootfs.raw`、virtio-blk）。
     seed diskがあれば2枚目を`readonly=on`で追加 |
   | `-netdev tap,...` `-device virtio-net-pci,...` | `internal/compute-agent/netsetup`が
     実際に作ったtapデバイスをそのまま使う（fcvmmと全く同じtap/ブリッジ配線。tap自体は
     どのVMMプロセスが繋がっても同じ） |
   | `-display none -serial stdio -monitor none -nodefaults` | ttyS0のみがI/O面。QEMU
     自身のstdout/stderrを`console.log`へ丸ごとリダイレクトすることで、ゲストのシリアル
     出力とQEMU自身の起動時エラーの両方を1ファイルで見られるようにする（fcvmmの
     `console.log`契約と同じ考え方） |

3. **成否判定**: fcvmm/qemuvmm共通の500ms猶予期間方式
   （[VirtualMachine仕様](virtual-machine.md)「起動確認（成否判定）」参照）。QEMU固有の
   即時失敗（バイナリが無い、`/dev/kvm`権限がない、カーネルイメージをQEMUの直接ブート
   ローダーが拒否する、等）を検知するためのもの

### boot_argsのデフォルトがFirecrackerと違う理由

FirecrackerはドライブAPIの`is_root_device`フラグを見て、ゲストのカーネルcmdlineへ
`root=/dev/vda`相当を**自動的に**注入する（ユーザーが明示しなくてよい）。QEMUには
この自動注入がないため、qemuvmm独自のデフォルト`boot_args`は`root=/dev/vda rw`を
明示的に含む:

```
console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/init
```

（fcvmmの`pci=off`は含まない——`q35`は実PCIバスを持つため、PCIプロービングを止めると
virtio-blk/virtio-netのPCIトランスポート自体が機能しなくなる）

同じ`KERNEL_ROOTFS`Imageを両ドライバで使い回せる、という前提は保たれている:
`spec.boot_args`を明示指定しない限り、それぞれのドライバが自分に必要な形へ自動的に
補ってくれる。

## UserData注入・cgroupリソース制限・削除・シリアルコンソール

すべて[VirtualMachine仕様](virtual-machine.md)の該当節と同じ仕組みを共有する
（`internal/compute-agent/vmm`のBuildSeedDisk、`internal/compute-agent/cgroup`、
`Stop`のSIGTERM→3秒待ちSIGKILL、`kyuusha vm console`）。qemuvmm固有の差分はない。

## playgroundでの構成

`docker/Dockerfile`の`compute-agent`ステージにQEMUパッケージを追加しただけ——
kernel/rootfsアセット自体はFirecracker用に既にある`image-assets`（vmlinux +
Alpine minirootfsのext4）をそのまま使う。`/dev/kvm`・tap配線・`privileged: true`・
`-drivers`フラグ（playgroundは`-drivers=FIRECRACKER,QEMU`——これが無いと
`driver_hint=QEMU`のVMはスケジュール不能になる）等、ドライバを問わず共通の要件は
[VirtualMachine仕様](virtual-machine.md)「playgroundでの共通構成」参照。
`playground/scenario.sh`は、Firecracker用の
Imageに対して`-driver-hint=qemu`で追加のVMを1台作り、同じ`kyuusha vm console`確認
（`docker/fc-guest-init.sh`が出す起動メッセージ。ドライバによらず同じ文言——ゲスト自身は
どちらのVMMで起動されたか区別できないため）で実ブートを確認する。

## この実装がカバーしないもの

- 上記「起動方式」で述べた通り、非Linuxゲスト（Windows等）とファームウェア依存機能
  （Secure Boot等）は原理的にサポート外——ブート可能ディスク経由の別実装が要る
- `QCOW2`形式のImageはまだどのドライバにも消費されない（`internal/compute`の
  Reconcilerが`spec.disk`を読んでいない。[Image仕様](image.md)参照）——将来の
  ブート可能ディスクサポートが実装されて初めて意味を持つ
- PCI passthrough (VFIO)・vhost-user networking: 上記「なぜQEMUも要るのか」で挙げた
  本来の動機そのものは、まだどちらも未実装（`-M q35`の選択だけがその布石）
- QEMU Monitor/QMPソケット経由の制御（一時停止、ライブマイグレーション等）: 使っていない
  （`-monitor none`）

ドライバを問わず共通の未実装事項（jailer相当の分離、digest検証、Stop/Restart、
`user_data`の機密情報対応、本物のcloud-initでの動作確認）は
[VirtualMachine仕様](virtual-machine.md)「この実装がカバーしないもの（共通）」参照。
