# Firecracker起動仕様

## 概要

`driver_hint=FIRECRACKER`（未指定時のデフォルト）のVirtualMachineは、compute-agentが実際に
Firecrackerプロセスを起動する。`VirtualMachineSpec.network_interfaces`が指定されたVMには
実際のtapデバイスも配線される（[network.md](network.md)「tap配線とローカルネットワーク」
参照）。`spec.vcpu`/`spec.memory_mb`はcgroup v2の`cpu.max`/`memory.max`として
host側でも強制される（`internal/compute-agent/cgroup`、[VirtualMachine仕様](virtual-machine.md)
「cgroupリソース制限」参照）。
Firecrackerプロセス自体は必ずjailer経由で起動する（chroot+uid/gid降格、下記
「jailer」参照）——compute-agentコンテナ自身の権限のまま動くことはない。
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
2. **jailの用意**: `-fc-jail-chroot-base-dir/firecracker/<vm_id>/root`
   （jailer自身が使うのと全く同じ計算式でこのパスを事前に求める）を作り、
   Firecrackerが参照するリソースを全てその中へ置く——kernelイメージのコピー
   （root所有、world-readable）、rootfsのコピー（jailのuid/gidへchown、書き込み
   可）、`spec.user_data`があればcloud-init seed diskをそのまま直接この中に生成、
   接続済みVolumeがあればその実デバイスと同じmajor:minorを持つブロックデバイス
   ノードを`mknod`（hard linkは使えない——iSCSIログインが作る実デバイスと
   このjailのディレクトリツリーは別ファイルシステムのため）。詳細は下記「jailer」参照
3. **起動**: jailの中に`config.json`（`boot-source`/`drives`/`machine-config`。
   パスはすべてjailからの相対パス）を書き、jailer経由でFirecrackerを起動する
   （下記「jailer」参照）。標準出力/標準エラー（＝シリアルコンソール`ttyS0`の出力）
   はjailの外、`<vm_id>/console.log`へ（すでに開いているファイルディスクリプタは
   chroot/pivot_rootの影響を受けないため、jailの中へ置く必要がない）
4. **成否判定**: プロセス起動から500ms以内に終了した場合のみ失敗とみなす、fcvmm/qemuvmm
   共通の方式（[VirtualMachine仕様](virtual-machine.md)「起動確認（成否判定）」参照）。

UserData注入・cgroupリソース制限・削除・シリアルコンソールアクセスも
[VirtualMachine仕様](virtual-machine.md)を参照（すべてfcvmm/qemuvmm共通）。

## jailer

`docs/architecture.md`「Firecracker: jailerとtapデバイス」が本番運用前に必須とした
jailer統合は**実装済み**: `driver_hint=FIRECRACKER`のVMは必ずFirecracker公式の
`jailer`バイナリ経由で起動し、chroot+uid/gid権限降格を得る。VMMエスケープ級の
脆弱性があった場合の被害範囲を、compute-agentコンテナ自身の全権限（root、
ホストの実デバイス、iSCSIイニシエータとして他テナントのストレージへ到達できる
可能性）から、chrootされた非root uid/gidの中だけに縮小する。

- **chroot + uid/gid降格のみ、namespace分離は無し**: `--netns`/`--new-pid-ns`は
  どちらも渡さない。この2つのVMのtap配線（[network.md](network.md)参照）と
  iSCSIイニシエータ側の`nsenter`（[Volume仕様](volume.md)参照）は、どちらも
  compute-agentコンテナ自身のnetwork namespace（または明示的にnsenterした先の
  ホストのnamespace）に依存しており、jailerへさらに別のnamespaceを重ねる設計は
  意図的にスコープ外とした（`--new-pid-ns`を渡さないため、jailerはFirecracker
  バイナリへそのままexecする——forkしない。よってcompute-agentが見るPIDは
  終始Firecracker自身のPIDのままで、cgroup適用やSIGTERM送出は変更なしで動く）
- **uid/gidは全VM共有の固定値**（`-fc-jail-uid`/`-fc-jail-gid`、既定`123`/`100`）
  ——VMごとに一意なuid/gidを割り当てる方式（同一ホスト上の別VM同士の分離も
  得られる）は将来の改善として保留。実在の`/etc/passwd`エントリである必要は
  ない（jailerもFirecrackerも生の数値uid/gidを`setuid`/`setgid`に渡すだけ）
- **jailerは指定した実行ファイル以外は何も持ち込まない**: `/dev/kvm`・
  `/dev/net/tun`はjailerが自動で`mknod`+chownしてくれるが、kernelイメージ・
  rootfsコピー・seed disk・Volumeのブロックデバイスノードは、jailerを呼ぶ前に
  呼び出し側（fcvmm）が自分でjailの中へ用意しておく必要がある（上記
  「compute-agent側の起動処理」参照）
- **seccompはjailerとは無関係にすでに有効**: Firecracker自体が`-no-seccomp`を
  明示しない限りデフォルトの組み込みseccompフィルタを適用する——jailerを使う
  かどうかに関係なく、このシステムはずっとこれが有効だった。jailerがこの機能
  に対して追加するものは無い（実際に貢献しているのはchroot+権限降格の部分だけ）
- `driver_hint=QEMU`には同等の隔離がまだ無い——`jailer`はFirecracker専用ツールで
  QEMUをラップできない。QEMU用の方式（自前chroot/namespace実装 vs. libvirt経由）
  は未決事項のまま（docs/open-questions.md参照）

## `-fc-*`フラグ（`cmd/compute-agent`）

| フラグ | 既定値 | 説明 |
|---|---|---|
| `-firecracker-bin` | `firecracker`（`$PATH`から解決） | jailerがexecするFirecrackerバイナリ |
| `-fc-cache-dir` | `/var/lib/kyuusha/fc-cache` | ダウンロード済みkernel/rootfsの共有キャッシュ |
| `-fc-run-dir` | `/var/lib/kyuusha/fc-run` | VMごとのconsole.log（それ以外は全てjailの中、下記参照） |
| `-fc-jailer-bin` | `jailer`（`$PATH`から解決） | 全`driver_hint=FIRECRACKER` VMが経由するjailerバイナリ |
| `-fc-jail-chroot-base-dir` | `/var/lib/kyuusha/fc-jail` | jailerの`--chroot-base-dir`。`<これ>/firecracker/<vm_id>/root`が各VMのjail |
| `-fc-jail-uid` / `-fc-jail-gid` | `123` / `100` | jailerが権限降格に使う、全VM共有の固定uid/gid |

## playgroundでの構成

`/dev/kvm`・tap配線・`privileged: true`・`-drivers`フラグ等、ドライバを問わず共通の
要件は[VirtualMachine仕様](virtual-machine.md)「playgroundでの共通構成」参照。
`jailer`バイナリはFirecracker本体と同じリリースtarballに同梱されており
（`docker/Dockerfile`のcompute-agentステージ）、追加のパッケージや
docker-compose.yml側の設定は要らない——jailer自身が要求するchroot/mount namespace
操作・`mknod`は、cgroup委譲のために元々`privileged: true`にしている範囲で
すでに賄える。Firecracker固有なのはkernel/rootfsアセットの中身:

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
