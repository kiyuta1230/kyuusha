# VirtualMachine仕様

## 概要

VirtualMachineは、実際にどのVMM（Virtual Machine Monitor）で起動されるかを
`spec.driver_hint`で選べるリソース。今のところ2つの実ドライバがある——
[Firecracker起動仕様](firecracker-boot.md)（`FIRECRACKER`、未指定時のデフォルト）と
[QEMU起動仕様](qemu-boot.md)（`QEMU`）。どちらも同じ`KERNEL_ROOTFS`形式のImage
（[Image仕様](image.md)参照）を、`internal/compute-agent`配下の別々のVMMドライバ
（`fcvmm`/`qemuvmm`）で起動する。

このドキュメントには、**ドライバを問わず共通の**仕組みだけをまとめる。ドライバ固有の
起動処理・引数・トレードオフはそれぞれのドキュメントを参照。`Pending`→`Scheduled`の
配置先決定自体は[VMスケジュール仕様](vm-scheduling.md)、NetworkInterfaceの実際の
tap配線は[network.md](network.md)、Volumeの実際の発見・配線は[Volume仕様](volume.md)
を参照。

## computeからcompute-agentへ渡る情報（`CreateCommand`）

compute-agentはImage/network/block-storageいずれのサービスクライアントも持たない。
VMが`Scheduled`→`Provisioning`へ遷移する際（[VMスケジュール仕様](vm-scheduling.md)
参照）、Reconcilerがそのタイミングでこれらを解決し、起動に必要な情報をすべて
`CreateCommand`（NATS、`internal/compute/nats.go`）に載せて渡す。ドライバを問わず
共通のペイロード——`driver_hint`が`FIRECRACKER`か`QEMU`かだけが変わる。

| フィールド | 由来 |
|---|---|
| `driver_hint` | `VirtualMachineSpec.driver_hint`（未指定なら`FIRECRACKER`） |
| `kernel_url` / `rootfs_url` | 解決したImageの`spec.kernel.url` / `spec.rootfs.url`（`QCOW2`の場合は空） |
| `boot_args` | Imageの`spec.boot_args`（空ならcompute-agent側のデフォルトを使う。デフォルト値自体はドライバごとに違う——[QEMU起動仕様](qemu-boot.md)「boot_argsのデフォルトがFirecrackerと違う理由」参照） |
| `interfaces` | `network_interfaces`から作られたNetworkInterface+そのSubnetの情報（[network.md](network.md)参照）。空配列ならネットワークなしで起動する |
| `volumes` | `volumes`から作られたVolumeAttachmentのうち、実際に`Attached`まで到達したものについて、そのVolume自身が持つ`protocol`/`storage_connection`/`identifier`（[Volume仕様](volume.md)参照。kyuushaはここで何もログイン/マウントしない——compute-agentが起動時にこの情報から既に見えているデバイス/ファイルを探すだけ）。空配列ならVolumeなしで起動する——アタッチが`Pending`のまま（排他制御待ち）だったものはここに含まれない |
| `user_data` | `VirtualMachineSpec.user_data`そのまま。空なら何も注入しない（下記「UserData注入」参照） |

`kernel_url`/`rootfs_url`が空、または`driver_hint`に対応する登録済みドライバがない場合
（`internal/compute-agent/agent.go`の`Drivers`マップに無い値）、compute-agentは
即座に成功を返す旧来のstub動作にフォールバックする。

## 起動確認（成否判定）

fcvmm/qemuvmm共通の方式（`bootGracePeriod`）: プロセス起動から500ms以内に終了した
場合のみ失敗（`CreateResult{Success: false}`）とみなす。この猶予時間は「バイナリが
無い」「/dev/kvm権限がない」「configが壊れている」等の即座に落ちる失敗を検知する
ためのもので、**ゲストカーネルが実際にブートし切ったことの確認ではない**（シリアル
コンソールの出力内容は見ない）。実際にブートしたかどうかは`console.log`を人手で
確認する運用（playgroundでは`scenario.sh`が起動メッセージの有無を確認する、下記
「シリアルコンソールアクセス」参照）。

## UserData注入（cloud-init NoCloud seed disk）

`spec.user_data`が空でないVMには、`internal/compute-agent/vmm/seed.go`（fcvmm/qemuvmm
共有）がcloud-initのNoCloud方式のseed diskを作り、root diskと並ぶ2番目の（読み取り専用の）
virtio-blockドライブとして渡す（ゲストからは通常`/dev/vdb`に見える）。
`docs/architecture.md`「UserData注入: NoCloud seed disk」参照。

- **中身**: `user-data`（`spec.user_data`そのまま、kyuushaは中身を検証・変換しない）、
  `meta-data`（`instance-id`/`local-hostname`に`vm_id`を使う——VMの`name`は冪等キーで
  空でありうるため）、そして`interfaces`の中にIP割当済みのものが1つでもあれば
  `network-config`（cloud-initのnetwork-config v2形式。IP/prefixとprimaryインタフェースの
  `gateway4`のみ、DNSサーバーは今のところ含めない）
- **生成方法**: `mkfs.ext4 -L cidata -d <ディレクトリ>`（`e2fsprogs`パッケージ、
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

## cgroupリソース制限（`internal/compute-agent/cgroup`）

fcvmm/qemuvmm共通の仕組み。VMMプロセスの起動直後（`cmd.Start()`成功後）、そのPIDを
`/sys/fs/cgroup/kyuusha/<VM ID>`というcgroup v2グループへ移し、`cpu.max`を
`<spec.vcpu>*100000 100000`（=vcpu個ぶんのフルコアを上限としたCPU quota）、
`memory.max`を`spec.memory_mb`をバイトに換算した値に設定する。VMMに渡した仮想
トポロジ（vcpu数/メモリサイズ）と全く同じ数値を、そのままhost側の実リソース上限
としても強制する形。

- **cgroup v2のunified hierarchyのみ対応**（v1は非対応）。`Available()`が偽を返す環境
  （cgroup v1のホスト、cgroupfsが読み取り専用/未委譲のコンテナ等）や、委譲はあっても
  controllerの有効化に失敗する環境では、`Apply`はエラーを返すだけで**VMの起動自体は
  ブロックしない**——警告ログを残し、そのVMは無制限リソースのまま起動を続ける
  （best-effort。以前の全バージョンと同じ挙動へのフォールバック）
- compute-agentコンテナ自身のプロセスは、cgroup v2の「no internal process」制約
  （`subtree_control`で子へcontrollerを委譲するには、そのcgroup自身の`cgroup.procs`が
  空でなければならない）を回避するため、`/sys/fs/cgroup/init`という兄弟cgroupへ
  自分自身を退避させてから`/sys/fs/cgroup`（cgroupnsで見えるcontainerの実質root）の
  `subtree_control`を有効化する（`cgroup.Init()`）。**`cmd/compute-agent`の起動時、
  最初のVMを起動するより前に一度だけ呼ぶ必要がある**——`Apply`の中で遅延実行すると、
  最初のVMのVMMプロセスがforkされる時点でcompute-agent自身がまだrootのcgroupに残って
  おり、その子プロセスもrootのcgroup.procsに入ったまま取り残されて`subtree_control`の
  有効化が恒久的にEBUSYで失敗する（ライブ検証で実際に踏んだ実バグ）
- VM終了時（VMMプロセスが実際に`wait(2)`され切った後）に`kyuusha/<VM ID>`ディレクトリを
  削除する。空にならないうちの削除はカーネルに拒否されるため、短い間隔でリトライする
- cgroupそのものは、jailerが本来提供する隔離（chroot/namespace/uid drop）ではない
  ——同一ホスト上の他プロセスからのファイルシステム上の可視性やnamespace分離は
  一切変わらず、あくまでCPU/メモリの消費量に上限を設けるだけ（docs/architecture.md
  「Firecracker: jailerとtapデバイス」参照）。`driver_hint=FIRECRACKER`は実際に
  jailerを使ってchroot+uid/gid降格を得ている（cgroupとは別の、fcvmm固有の仕組み
  ——[Firecracker起動仕様](firecracker-boot.md)「jailer」参照）が、
  `driver_hint=QEMU`にはまだ同等のものがない（QEMU用の隔離方式自体が
  docs/open-questions.mdの未決事項）

## 停止/起動（`Stop`/`Start`、2026-09-12実装）

`VirtualMachineService.Stop(vm_id, force)`/`Start(vm_id)`の2 RPCのみ追加。
`reboot`/`hard-reboot`はサーバー側に対応するRPCや状態を一切持たない、CLIだけの
組み合わせ（[CLI仕様](cli.md)参照）。

- **Stop**: `Running`のみ許可（それ以外は`FailedPrecondition`）。`Stopping`へ遷移し、
  Reconcilerが`StopCommand{vm_id, force}`をcompute-agentへpublishする。agentは
  登録済みの全VMMドライバへ`Stop(vmID, force)`を試す（`Destroy`と同様、実際に
  そのVMを起動していたドライバ以外は無害なno-op）。`force=false`ならSIGTERM→
  ドライバ固有の猶予期間→SIGKILL、`force=true`なら即SIGKILLで、**いずれもプロセスの
  実終了を待ってから**`vm.stop-result`イベントで結果を返す（`DeleteCommand`と異なり
  結果イベントが要る——Stopping→Stoppedの遷移が実際の終了確認に依存するため）。
  成功で`Stopped`、失敗（終了を確認できなかった）は`Running`へ差し戻し、
  `StopFailed`conditionを記録する。tap/volumeマウント等の後始末はプロセス終了時の
  既存の仕組みがそのまま走るが、**jail/runディレクトリ（根本ディスクの実体）は
  削除しない**——それがStopとDelete/Destroyの唯一の違い。
- **Start**: `Stopped`のみ許可。`Starting`という純粋に一時的なphaseを経て、
  reconcile()は`Scheduled`と全く同じ処理（`provisionAndPublish`）を流用する。
  新規のNATSコマンド種別は無く、既存の`CreateCommand`をそのまま再構築して送るだけ
  （`status.interface_refs`/`volume_attachment_refs`は既に保持済みなので、
  NetworkInterface/VolumeAttachmentは同名で再Create＝冪等に再取得されるだけで実際の
  新規作成は起きない）。compute-agent側の`Boot()`は、対象VM IDの書き込み可能rootfs
  コピーが既に存在すればそれをそのまま再利用し（Imageからの再コピーをスキップ）、
  存在しなければ通常のCreate同様に新規コピーする、という1分岐が入っているだけ。
  Firecracker側（`internal/compute-agent/fcvmm`）はさらに、jailer自身が
  「既にセットアップ済みのchroot」を受け付けない（`/dev/net/tun`等のmknodが
  `EEXIST`で失敗する）ため、rootfsだけを外へ退避してchroot全体を作り直し、
  rootfsだけ元へ戻す、という一手間が要る。

## 削除

VMが削除されると、Reconcilerは（Hypervisor容量の解放と同時に）`DeleteCommand`を
fire-and-forgetでpublishする（結果イベントなし——VM削除自体はこれの完了を待たない）。
compute-agentは該当VM IDについて、登録済みの全VMMドライバの`Destroy(vmID)`を試す
（実際にそのVMを起動していたドライバ以外は無害なno-op）——プロセスを停止（SIGKILL）
した上で、jail/runディレクトリそのものを削除する。`Stop`と違い根本ディスクの実体も
ここで消える（上記「停止/起動」節参照）。2026-09-12より前は`Stop`相当の処理しか
呼んでおらず、Delete後もディスクの実体がホスト側に永久にリークし続ける実バグが
あった（`Destroy`という別メソッドを新設して修正）。stub経路で一度も実プロセスを
起動していないVMのDeleteCommandは無害（何もしない）。

`DeleteCommand`のpublishに続けて、そのVMが持っていた全VolumeAttachmentも
fire-and-forgetで削除する（[Volume仕様](volume.md)参照）——これをしないと、そのVolumeは
`Attached`のまま残り続け、排他制御（docs/architecture.md「具体的な排他制御」）に阻まれて
永久に別のVMへ再アタッチできなくなる。**VolumeAttachment削除はDeleteCommandの後**——
先に削除するとまだ動いているかもしれないゲストの下からディスクを引き抜くことになる
ため、まずcompute-agentへ停止の先手を打ってから外す（それでも同期的な完了待ちはせず、
tap/cgroup後始末と同じeventual-consistency）。

NetworkInterfaceはVM削除時に一切触れない（オーファンとして残る）——[network.md](network.md)
「NetworkInterfaceのオーファンGC」参照。VolumeAttachmentと違って明示的な排他制御を
妨げないため、まだGCの実装優先度が上がっていない。

## シリアルコンソールアクセス（`VirtualMachineService.StreamConsole`）

`kyuusha vm console -tenant=... -id=... [-tail-bytes=N] [-follow]`で、`<vm_id>/console.log`
（＝VMMプロセスの標準出力＝ゲストのシリアルコンソール`ttyS0`）を読める。SSHのような
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

## playgroundでの共通構成

ドライバ固有の要件（Firecrackerバイナリの取得、QEMUパッケージのインストール等）は
それぞれの仕様書を参照。ここに書くのはドライバを問わず共通の要件だけ:

- `/dev/kvm`をcompute-agentコンテナへ渡す必要がある（`playground/docker-compose.yml`の
  `compute-agent-1/2/3`は host の`/dev`を丸ごとbind mountしている——[Volume仕様](volume.md)
  の「iSCSI initiator」節が要求する実ブロックデバイスの可視性と共用、詳細はそちら参照）。
  ホストにKVMがない場合、どちらのドライバもVMの起動自体が失敗する（スタックの他の部分は
  影響を受けない）。同様に、tap配線（[network.md](network.md)参照、どちらのドライバも
  同じtapデバイスをそのまま使う）には`/dev/net/tun`と`CAP_NET_ADMIN`が要る——なければ
  tap配線だけが失敗する
- cgroupリソース制限（上記）を実際に効かせるには`privileged: true`が要る——Docker/runcは
  非privilegedコンテナのcgroupfsを常にread-onlyでマウントし、`CAP_SYS_ADMIN`を個別に
  付与するだけでは書き込み可能にならない（ライブ検証で確認済み）。無くてもVMの起動自体は
  失敗しない（best-effortでリソース無制限のまま起動を続けるだけ）
- `-drivers`フラグで、そのcompute-agentがサポートするVMMドライバをスケジューラへ申告する
  （`compute-agent-1/2/3`の`command:`）。playgroundは`-drivers=FIRECRACKER,QEMU`——これが
  無い（既定値`FIRECRACKER`のみ）と、`driver_hint=QEMU`のVMはスケジュール可能な
  Hypervisorが1台も無い状態になり、`Pending`のまま進まなくなる（[VMスケジュール仕様](vm-scheduling.md)
  参照。ライブ検証で実際に踏んだ実バグ）
- kernel/rootfsは`image-assets`という専用compose serviceが配信する（プレーンHTTP、ホストには
  公開しない）。ドライバを問わず同じアセットを共有する——[Firecracker起動仕様](firecracker-boot.md)
  「playgroundでの構成」参照

## この実装がカバーしないもの（共通）

- ダウンロードした`kernel_url`/`rootfs_url`の内容のdigest検証
- Stop（一時停止）/Restart。今あるのは起動（Boot）と削除に伴う強制終了（Stop=プロセス終了）のみ
- `user_data`の機密情報対応（保存時暗号化、監査ログからの除外。
  `docs/architecture.md`「UserData注入」の「機密情報の扱いに関する注記」参照）
- 本物のcloud-initを動かすゲストでの動作確認（playgroundの最小Alpineゲストには
  cloud-init自体が入っていないため、seed diskが正しく届くことまでしか確認していない）

jailer相当のプロセス隔離はもう「共通の未実装事項」ではない——`driver_hint=FIRECRACKER`
は実装済み（[Firecracker起動仕様](firecracker-boot.md)「jailer」参照）、
`driver_hint=QEMU`は未実装のまま（docs/open-questions.md参照）と、ドライバごとに
状況が分かれている。その他のドライバ固有の未実装事項（例: Firecrackerのクロス
hypervisorネットワーク疎通、QEMUのPCI passthrough/vhost-user）も、それぞれの
仕様書の「この実装がカバーしないもの」を参照。
