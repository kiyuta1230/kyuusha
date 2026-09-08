# Firecracker起動仕様

## 概要

`driver_hint=FIRECRACKER`（未指定時のデフォルト）のVirtualMachineは、compute-agentが実際に
Firecrackerプロセスを起動する。`VirtualMachineSpec.network_interfaces`が指定されたVMには
実際のtapデバイスも配線される（[network.md](network.md)「tap配線とローカルネットワーク」
参照）。`spec.vcpu`/`spec.memory_mb`はcgroup v2の`cpu.max`/`memory.max`として
host側でも強制される（`internal/compute-agent/cgroup`、下記「cgroupリソース制限」参照）。
それ以外の部分は今も**jailerなし**の最小スコープのまま——jailer本来の
chroot+namespace分離+特権降格は意図的に見送っており、Firecrackerプロセスは
compute-agentコンテナの権限のまま動く（**本番の隔離設計はdocs/architecture.mdの
「Firecracker: jailerとtapデバイス」節を参照。ここに書くのはあくまで現状の実装**）。
`driver_hint=QEMU`は別ドライバとして実装済み——[QEMU起動仕様](qemu-boot.md)参照
（同じ`KERNEL_ROOTFS`形式のImageを、`internal/compute-agent/qemuvmm`が別のVMMプロセスで
起動する）。

## computeからcompute-agentへ渡る情報

compute-agentはImageサービスのクライアントを持たない。VMが`Scheduled`→`Provisioning`へ
遷移する際（[VMスケジュール仕様](vm-scheduling.md)参照）、Reconcilerがそのタイミングで
Imageを解決し、起動に必要な情報をすべて`CreateCommand`（NATS）に載せて渡す。

| フィールド | 由来 |
|---|---|
| `driver_hint` | `VirtualMachineSpec.driver_hint`（未指定なら`FIRECRACKER`） |
| `kernel_url` / `rootfs_url` | 解決したImageの`spec.kernel.url` / `spec.rootfs.url`（`QCOW2`の場合は空） |
| `boot_args` | Imageの`spec.boot_args`（空ならcompute-agent側のデフォルトを使う） |
| `interfaces` | `network_interfaces`から作られたNetworkInterface+そのSubnetの情報（[network.md](network.md)参照）。空配列ならネットワークなしで起動する |
| `user_data` | `VirtualMachineSpec.user_data`そのまま。空なら何も注入しない（下記「UserData注入」参照） |

`kernel_url`/`rootfs_url`が空、または`driver_hint`に対応する登録済みドライバがない場合
（`internal/compute-agent/agent.go`の`Drivers`マップに無い値）、compute-agentは
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

## UserData注入（cloud-init NoCloud seed disk）

`spec.user_data`が空でないVMには、`internal/compute-agent/vmm/seed.go`（fcvmm/qemuvmm
共有）がcloud-initのNoCloud方式のseed diskを作り、root diskと並ぶ2番目の（読み取り専用の）
virtio-blockドライブとしてFirecrackerへ渡す（ゲストからは通常`/dev/vdb`に見える）。
`docs/architecture.md`「UserData注入: NoCloud seed disk」参照。

- **中身**: `user-data`（`spec.user_data`そのまま、kyuushaは中身を検証・変換しない）、
  `meta-data`（`instance-id`/`local-hostname`に`vm_id`を使う——VMの`name`は冪等キーで
  空でありうるため）、そして`interfaces`の中にIP割当済みのものが1つでもあれば
  `network-config`（cloud-initのnetwork-config v2形式。IP/prefixとprimaryインタフェースの
  `gateway4`のみ、DNSサーバーは今のところ含めない）
- **生成方法**: `mkfs.ext4 -L cidata -d <ディレクトリ>`（Alpineの`e2fsprogs`パッケージ、
  compute-agentイメージにインストール済み）。フォーマットとファイル投入を1コマンドで
  やる、`docker/Dockerfile`の`image-assets`ステージがrootfs自体を作るのに使っているのと
  同じ手法。**当初iso9660（`genisoimage`）、次にvfat（`mtools`）で実装したが、
  playgroundが使うFirecracker CI配布カーネルにはそのどちらのファイルシステムも
  （`CONFIG_ISO9660_FS`も`CONFIG_VFAT_FS`も）入っておらずゲスト側でマウントできない
  ことがライブ検証で2回とも判明し、確実に動くext4に切り替えた**。cloud-initの
  NoCloudデータソースは`blkid`でラベルを見つけたあとファイルシステム型を指定せず
  汎用マウントするため、ext4でも（vfat/iso9660限定ではなく）実運用のcloud-initから
  問題なく読める
- **前提**: ゲスト側`Image`のrootfsに実際のcloud-initが入っていること
  （イメージビルド側の責務、kyuushaは強制しない）
- **playgroundでの検証**: playgroundの最小自作Alpineゲストには実際のcloud-initが
  入っていない。そのため`docker/fc-guest-init.sh`（PID 1のスタンドイン）が、
  `/dev/vdb`が存在すればext4としてマウントし、`user-data`の中身をそのまま
  シリアルコンソールへ書き出す——**本物のcloud-initを動かしているわけではなく**、
  seed diskが正しく生成・接続され読めることだけを確認する自己診断（tap配線の
  gateway ping自己診断と同じ考え方）

## 削除

VMが削除されると、Reconcilerは（容量解放と同時に）`DeleteCommand`をfire-and-forgetで
publishする（結果イベントなし -- VM削除自体はこれの完了を待たない）。compute-agentは
該当VMIDのFirecrackerプロセスにSIGTERMを送り、3秒待ってSIGKILLする。stub経路で一度も
実プロセスを起動していないVMのDeleteCommandは無害（何もしない）。

## シリアルコンソールアクセス（`VirtualMachineService.StreamConsole`）

`kyuusha vm console -tenant=... -id=... [-tail-bytes=N] [-follow]`で、`<vm_id>/console.log`
（＝Firecrackerの標準出力＝ゲストのシリアルコンソール`ttyS0`）を読める。SSHのような
インタラクティブなゲストアクセス手段がまだ無いこの実装では、ゲストの状態を外から確認する
唯一の手段（`docs/specs/audit-logging.md`のような監査目的ではなく、デバッグ目的）。実際、
tap配線（[network.md](network.md)参照）が正しく効いているかどうか自体もこのコンソール
出力（ゲストの`/init`が書くping結果の行）で確認する。

- **既定**: 末尾64KiB相当を返して終了（`-tail-bytes`未指定時）。`-tail-bytes`に負の値を
  渡すとログ全体、正の値を渡すとその バイト数分の末尾を返す
- **`-follow`**: 既存分を返した後、`tail -f`同様に新規出力をストリームし続ける。クライアント
  が切断すると即座に止まる（Reconcilerが停止シグナルをpublishする）。念のためcompute-agent側
  にも30分の安全上限がある（[NATSメッセージ仕様](nats-messaging.md)参照）

対象VMが一度もスケジュールされていない（`status.hypervisor`が空）場合や、
（stub-succeededなVM等）実プロセスを一度も起動していない場合はエラーになる。

## `-fc-*`フラグ（`cmd/compute-agent`）

| フラグ | 既定値 | 説明 |
|---|---|---|
| `-firecracker-bin` | `firecracker`（`$PATH`から解決） | 実行するFirecrackerバイナリ |
| `-fc-cache-dir` | `/var/lib/kyuusha/fc-cache` | ダウンロード済みkernel/rootfsの共有キャッシュ |
| `-fc-run-dir` | `/var/lib/kyuusha/fc-run` | VMごとの書き込み可能rootfsコピー・APIソケット・console.log |

## cgroupリソース制限（`internal/compute-agent/cgroup`）

fcvmm/qemuvmm共通の仕組み（下記は両方に当てはまる。qemuvmm側の適用は
[QEMU起動仕様](qemu-boot.md)参照）。Firecrackerプロセスの起動直後（`cmd.Start()`成功後）、そのPIDを`/sys/fs/cgroup/kyuusha/<VM ID>`
というcgroup v2グループへ移し、`cpu.max`を`<spec.vcpu>*100000 100000`（=vcpu個ぶんのフル
コアを上限としたCPU quota）、`memory.max`を`spec.memory_mb`をバイトに換算した値に設定する。
Firecracker/KVMに渡した仮想トポロジ（`machine-config`のvcpu_count/mem_size_mib）と全く同じ
数値を、そのままhost側の実リソース上限としても強制する形。

- **cgroup v2のunified hierarchyのみ対応**（v1は非対応）。`Available()`が偽を返す環境
  （cgroup v1のホスト、cgroupfsが読み取り専用/未委譲のコンテナ等）や、委譲はあっても
  controllerの有効化に失敗する環境では、`Apply`はエラーを返すだけで**VMの起動自体は
  ブロックしない**——警告ログを残し、そのVMは無制限リソースのまま起動を続ける
  （best-effort。以前の全バージョンと同じ挙動へのフォールバック）
- compute-agentコンテナ自身のプロセスは、cgroup v2の「no internal process」制約
  （`subtree_control`で子へcontrollerを委譲するには、そのcgroup自身の`cgroup.procs`が
  空でなければならない）を回避するため、起動時に`/sys/fs/cgroup/init`という兄弟cgroupへ
  自分自身を退避させてから`/sys/fs/cgroup`（cgroupnsで見えるcontainerの実質root）の
  `subtree_control`を有効化する（`ensureSelfMoved`）
- VM終了時（Firecrackerプロセスが実際に`wait(2)`され切った後）に`kyuusha/<VM ID>`
  ディレクトリを削除する。空にならないうちの削除はカーネルに拒否されるため、
  短い間隔でリトライする
- jailerが本来提供する隔離（chroot/namespace/uid drop）そのものではない——同一ホスト上の
  他プロセスからのファイルシステム上の可視性やnamespace分離は一切変わらず、あくまで
  CPU/メモリの消費量に上限を設けるだけ（docs/architecture.md「Firecracker: jailerとtap
  デバイス」参照）

## playgroundでの構成

- `/dev/kvm`をcompute-agentコンテナへ渡す必要がある（`playground/docker-compose.yml`の
  `compute-agent-1/2/3`の`devices:`）。ホストにKVMがない場合、Firecrackerの起動自体が
  失敗する（スタックの他の部分は影響を受けない）。同様に、tap配線には`/dev/net/tun`と
  `CAP_NET_ADMIN`が要る（同じ`devices:`/`cap_add:`）——なければtap配線だけが失敗する
- kernel/rootfsは`image-assets`という専用compose serviceが配信する（プレーンHTTP、ホストには
  公開しない）。中身はビルド時（`docker build`。このsandboxではコンテナのランタイムネット
  ワークが外部インターネットに届かないため、実行時ではなくビルド時に取得している）に用意する:
  Firecracker公式CIが配布するvmlinux（`docker/Dockerfile`の`image-assets`ステージ参照）と、
  Alpine minirootfsを土台にした自前rootfs（`docker/fc-guest-init.sh`をPID 1として動かす。
  Alpineの`/sbin/init`（openrc前提）は完全ではないため、`boot_args`に`init=/init`を必須とする
  -- compute-agent側のデフォルト`boot_args`には最初から含まれている）
- `playground/scenario.sh`が作るImageはこのkernel/rootfsを指す。VM作成後、
  `kyuusha vm console`で実際に起動確認メッセージが読めることを確認する

## この実装がカバーしないもの

- クロスHypervisorのネットワーク疎通（同じHypervisor内のtap+ブリッジのみ。
  [network.md](network.md)「tap配線とローカルネットワーク」参照）
- jailer本来のchroot/namespace分離・特権降格（cgroupによるCPU/メモリ制限のみ実装済み、
  上記「cgroupリソース制限」参照。本番運用前に必須、docs/architecture.md参照）
- ダウンロードした`kernel_url`/`rootfs_url`の内容のdigest検証
- Stop（一時停止）/Restart。今あるのは起動（Boot）と削除に伴う強制終了（Stop=プロセス終了）のみ
- `user_data`の機密情報対応（保存時暗号化、監査ログからの除外。
  `docs/architecture.md`「UserData注入」の「機密情報の扱いに関する注記」参照）
- 本物のcloud-initを動かすゲストでの動作確認（playgroundの最小Alpineゲストには
  cloud-init自体が入っていないため、seed diskが正しく届くことまでしか確認していない）
