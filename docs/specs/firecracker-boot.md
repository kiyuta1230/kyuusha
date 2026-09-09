# Firecracker起動仕様

## 概要

`driver_hint=FIRECRACKER`（未指定時のデフォルト）のVirtualMachineは、compute-agentが実際に
Firecrackerプロセスを起動する。`VirtualMachineSpec.network_interfaces`が指定されたVMには
実際のtapデバイスも配線される（[network.md](network.md)「tap配線とローカルネットワーク」
参照）。`spec.vcpu`/`spec.memory_mb`はcgroup v2の`cpu.max`/`memory.max`として
host側でも強制される（`internal/compute-agent/cgroup`、[VirtualMachine仕様](virtual-machine.md)
「cgroupリソース制限」参照）。
それ以外の部分は今も**jailerなし**の最小スコープのまま——jailer本来の
chroot+namespace分離+特権降格は意図的に見送っており、Firecrackerプロセスは
compute-agentコンテナの権限のまま動く（**本番の隔離設計はdocs/architecture.mdの
「Firecracker: jailerとtapデバイス」節を参照。ここに書くのはあくまで現状の実装**）。
`driver_hint=QEMU`は別ドライバとして実装済み——[QEMU起動仕様](qemu-boot.md)参照
（同じ`KERNEL_ROOTFS`形式のImageを、`internal/compute-agent/qemuvmm`が別のVMMプロセスで
起動する）。

## computeからcompute-agentへ渡る情報

ドライバを問わず共通の`CreateCommand`ペイロード（stubフォールバックの条件含む）——
[VirtualMachine仕様](virtual-machine.md)「computeからcompute-agentへ渡る情報」参照。

## compute-agent側の起動処理（`internal/compute-agent/fcvmm`）

1. **キャッシュ確認**: `kernel_url`/`rootfs_url`それぞれをURL文字列のSHA-256でキー化し、
   `-fc-cache-dir`（既定`/var/lib/kyuusha/fc-cache`）に未取得ならHTTP GETで取得する。
   同一Imageから起動する複数VMで共有される（**digest検証はしない** -- Imageの`digest`
   フィールドはオプショナルなため。[Image仕様](image.md)の「digest検証は誰が読む時にするか」
   の方針通り、ここでも検証しない）
2. **rootfsの複製**: Firecrackerはrootドライブを読み書きで開き、ゲストの変更をそのまま
   バックエンドファイルに書き込む。複数VMがキャッシュされたマスタを直接共有すると壊れるため、
   VMごとに`-fc-run-dir/<vm_id>/rootfs.ext4`へコピーしてから渡す（reflinkは使わず素朴な
   コピー。CoW最適化は将来の改善余地）
3. **起動**: `<vm_id>/config.json`（`boot-source`/`drives`/`machine-config`）を書き、
   `firecracker --api-sock <vm_id>/api.sock --config-file <vm_id>/config.json`を実行。
   標準出力/標準エラー（＝シリアルコンソール`ttyS0`の出力）は`<vm_id>/console.log`へ
4. **成否判定**: プロセス起動から500ms以内に終了した場合のみ失敗とみなす、fcvmm/qemuvmm
   共通の方式（[VirtualMachine仕様](virtual-machine.md)「起動確認（成否判定）」参照）。

UserData注入・cgroupリソース制限・削除・シリアルコンソールアクセスも
[VirtualMachine仕様](virtual-machine.md)を参照（すべてfcvmm/qemuvmm共通）。

## `-fc-*`フラグ（`cmd/compute-agent`）

| フラグ | 既定値 | 説明 |
|---|---|---|
| `-firecracker-bin` | `firecracker`（`$PATH`から解決） | 実行するFirecrackerバイナリ |
| `-fc-cache-dir` | `/var/lib/kyuusha/fc-cache` | ダウンロード済みkernel/rootfsの共有キャッシュ |
| `-fc-run-dir` | `/var/lib/kyuusha/fc-run` | VMごとの書き込み可能rootfsコピー・APIソケット・console.log |

## playgroundでの構成

`/dev/kvm`・tap配線・`privileged: true`・`-drivers`フラグ等、ドライバを問わず共通の
要件は[VirtualMachine仕様](virtual-machine.md)「playgroundでの共通構成」参照。
Firecracker固有なのはkernel/rootfsアセットの中身:

- kernel/rootfsは`image-assets`という専用compose serviceが配信する（プレーンHTTP、ホストには
  公開しない）。中身はビルド時（`docker build`。このsandboxではコンテナのランタイムネット
  ワークが外部インターネットに届かないため、実行時ではなくビルド時に取得している）に用意する:
  Firecracker公式CIが配布するvmlinux（`docker/Dockerfile`の`image-assets`ステージ参照）と、
  Alpine minirootfsを土台にした自前rootfs（`docker/fc-guest-init.sh`をPID 1として動かす。
  Alpineの`/sbin/init`（openrc前提）は完全ではないため、`boot_args`に`init=/init`を必須とする
  -- compute-agent側のデフォルト`boot_args`には最初から含まれている）。このアセットは
  `driver_hint=QEMU`のVMもそのまま使い回す（[QEMU起動仕様](qemu-boot.md)参照）
- `playground/scenario.sh`が作るImageはこのkernel/rootfsを指す。VM作成後、
  `kyuusha vm console`で実際に起動確認メッセージが読めることを確認する

## この実装がカバーしないもの

- クロスHypervisorのネットワーク疎通（同じHypervisor内のtap+ブリッジのみ。
  [network.md](network.md)「tap配線とローカルネットワーク」参照）

ドライバを問わず共通の未実装事項（jailer相当の分離、digest検証、Stop/Restart、
`user_data`の機密情報対応、本物のcloud-initでの動作確認）は
[VirtualMachine仕様](virtual-machine.md)「この実装がカバーしないもの（共通）」参照。
