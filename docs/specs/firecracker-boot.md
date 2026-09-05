# Firecracker起動仕様

## 概要

`driver_hint=FIRECRACKER`（未指定時のデフォルト）のVirtualMachineは、compute-agentが実際に
Firecrackerプロセスを起動する。この最初の実装は**ネットワークデバイスなし・jailerなし**の
最小スコープに限定している。tap配線/VLANはnetworkサービス（未実装）に依存するため、それが
できるまでは意図的に外してある。jailer（chroot+cgroup+namespace分離）も同様に見送っており、
Firecrackerプロセスはcompute-agentコンテナの権限のまま動く（**本番の隔離設計は
docs/architecture.mdの「Firecracker: jailerとtapデバイス」節を参照。ここに書くのはあくまで
現状の実装**）。`driver_hint=QEMU`は引き続き未実装（stub-success）のまま。

## computeからcompute-agentへ渡る情報

compute-agentはImageサービスのクライアントを持たない。VMが`Scheduled`→`Provisioning`へ
遷移する際（[VMスケジュール仕様](vm-scheduling.md)参照）、Reconcilerがそのタイミングで
Imageを解決し、起動に必要な情報をすべて`CreateCommand`（NATS）に載せて渡す。

| フィールド | 由来 |
|---|---|
| `driver_hint` | `VirtualMachineSpec.driver_hint`（未指定なら`FIRECRACKER`） |
| `kernel_url` / `rootfs_url` | 解決したImageの`spec.kernel.url` / `spec.rootfs.url`（`QCOW2`の場合は空） |
| `boot_args` | Imageの`spec.boot_args`（空ならcompute-agent側のデフォルトを使う） |

`kernel_url`/`rootfs_url`が空、または`driver_hint`が`FIRECRACKER`以外の場合、compute-agentは
即座に成功を返す旧来のstub動作にフォールバックする。

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
4. **成否判定**: プロセス起動から500ms以内に終了した場合のみ失敗（`CreateResult{Success:
   false}`）とみなす。この猶予時間は「バイナリが無い」「/dev/kvm権限がない」「configが壊れて
   いる」等の即座に落ちる失敗を検知するためのもので、**ゲストカーネルが実際にブートし切った
   ことの確認ではない**（シリアルコンソールの出力内容は見ない）。実際にブートしたかどうかは
   `console.log`を人手で確認する運用（playgroundでは`scenario.sh`が起動メッセージの有無を
   確認する）

## 削除

VMが削除されると、Reconcilerは（容量解放と同時に）`DeleteCommand`をfire-and-forgetで
publishする（結果イベントなし -- VM削除自体はこれの完了を待たない）。compute-agentは
該当VMIDのFirecrackerプロセスにSIGTERMを送り、3秒待ってSIGKILLする。stub経路で一度も
実プロセスを起動していないVMのDeleteCommandは無害（何もしない）。

## `-fc-*`フラグ（`cmd/compute-agent`）

| フラグ | 既定値 | 説明 |
|---|---|---|
| `-firecracker-bin` | `firecracker`（`$PATH`から解決） | 実行するFirecrackerバイナリ |
| `-fc-cache-dir` | `/var/lib/kyuusha/fc-cache` | ダウンロード済みkernel/rootfsの共有キャッシュ |
| `-fc-run-dir` | `/var/lib/kyuusha/fc-run` | VMごとの書き込み可能rootfsコピー・APIソケット・console.log |

## playgroundでの構成

- `/dev/kvm`をcompute-agentコンテナへ渡す必要がある（`playground/docker-compose.yml`の
  `compute-agent-1/2/3`の`devices:`）。ホストにKVMがない場合、Firecrackerの起動自体が
  失敗する（スタックの他の部分は影響を受けない）
- kernel/rootfsは`image-assets`という専用compose serviceが配信する（プレーンHTTP、ホストには
  公開しない）。中身はビルド時（`docker build`。このsandboxではコンテナのランタイムネット
  ワークが外部インターネットに届かないため、実行時ではなくビルド時に取得している）に用意する:
  Firecracker公式CIが配布するvmlinux（`docker/Dockerfile`の`image-assets`ステージ参照）と、
  Alpine minirootfsを土台にした自前rootfs（`docker/fc-guest-init.sh`をPID 1として動かす。
  Alpineの`/sbin/init`（openrc前提）は完全ではないため、`boot_args`に`init=/init`を必須とする
  -- compute-agent側のデフォルト`boot_args`には最初から含まれている）
- `playground/scenario.sh`が作るImageはこのkernel/rootfsを指す。VM作成後、該当ホストの
  compute-agentコンテナに入って`console.log`に起動確認メッセージがあることを確認する

## この実装がカバーしないもの

- ネットワーク（tap/VLAN。networkサービス実装後の別途対応）
- jailer（chroot/cgroup/namespace分離。本番運用前に必須、docs/architecture.md参照）
- シリアルコンソールのライブストリーミング（`console.log`はファイルに書きっぱなしで、
  gRPCストリーム越しに配信する機能はまだない）
- ダウンロードした`kernel_url`/`rootfs_url`の内容のdigest検証
- Stop（一時停止）/Restart。今あるのは起動（Boot）と削除に伴う強制終了（Stop=プロセス終了）のみ
