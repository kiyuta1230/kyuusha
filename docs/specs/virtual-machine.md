# VirtualMachine仕様

## 概要

VirtualMachineは、実際にどのVMM（Virtual Machine Monitor）で起動されるかを
`spec.driver_hint`で選べるリソース。今のところ2つの実ドライバがある——
[Firecracker起動仕様](firecracker-boot.md)（`FIRECRACKER`、未指定時のデフォルト）と
[cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)（`CLOUD_HYPERVISOR`）。
`KERNEL_ROOTFS`形式のImage（[Image仕様](image.md)参照）はどちらのドライバでも
起動できる（`internal/compute-agent`配下の別々のVMMドライバ`fcvmm`/`chvmm`）。
`QCOW2`形式のImageは`chvmm`のみが対応（UEFIブート、2026-09-15実装——
[cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)「起動方式2: UEFIブート」
参照）——`fcvmm`は構造的にブート可能ディスクを起動できないため、Create時に
`driver_hint=CLOUD_HYPERVISOR`が強制される。

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
共通のペイロード——`driver_hint`が`FIRECRACKER`か`CLOUD_HYPERVISOR`かだけが変わる。

| フィールド | 由来 |
|---|---|
| `driver_hint` | `VirtualMachineSpec.driver_hint`（未指定なら`FIRECRACKER`） |
| `kernel_url` / `rootfs_url` | 解決したImageの`spec.kernel.url` / `spec.rootfs.url`（`QCOW2`の場合は空） |
| `kernel_digest` / `rootfs_digest` | 解決したImageの`spec.kernel.digest` / `spec.rootfs.digest`。compute-agentの`internal/compute-agent/imagestore`がダウンロード後のバイト列をこれと照合する（空の場合は未検証のまま従来通りキャッシュする。[Firecracker起動仕様](firecracker-boot.md)「compute-agent側の起動処理」参照） |
| `disk_url` / `disk_digest` | 解決したImageの`spec.disk.url` / `spec.disk.digest`（`KERNEL_ROOTFS`の場合は空。`kernel_url`/`rootfs_url`と同時に非空になることはない）。`chvmm`のみが消費する——[cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)「起動方式2: UEFIブート」参照 |
| `boot_args` | Imageの`spec.boot_args`（空ならcompute-agent側のデフォルトを使う。デフォルト値自体はドライバごとに違う——[cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)「boot_argsのデフォルトがFirecrackerと違う理由」参照） |
| `interfaces` | `network_interfaces`から作られたNetworkInterface+そのSubnetの情報（[network.md](network.md)参照）。空配列ならネットワークなしで起動する |
| `volumes` | `volumes`から作られたVolumeAttachmentのうち、実際に`Attached`まで到達したものについて、そのVolume自身が持つ`protocol`/`storage_connection`/`identifier`（[Volume仕様](volume.md)参照。kyuushaはここで何もログイン/マウントしない——compute-agentが起動時にこの情報から既に見えているデバイス/ファイルを探すだけ）。空配列ならVolumeなしで起動する——アタッチが`Pending`のまま（排他制御待ち）だったものはここに含まれない |
| `user_data` | `VirtualMachineSpec.user_data`そのまま。空なら何も注入しない（下記「UserData注入」参照） |

`kernel_url`+`rootfs_url`と`disk_url`のどちらも空、または`driver_hint`に対応する
登録済みドライバがない場合（`internal/compute-agent/agent.go`の`Drivers`マップに
無い値）、compute-agentは即座に成功を返す旧来のstub動作にフォールバックする。

## 起動確認（成否判定）

fcvmm/chvmm共通の方式（`bootGracePeriod`）: プロセス起動から500ms以内に終了した
場合のみ失敗（`CreateResult{Success: false}`）とみなす。この猶予時間は「バイナリが
無い」「/dev/kvm権限がない」「configが壊れている」等の即座に落ちる失敗を検知する
ためのもので、**ゲストカーネルが実際にブートし切ったことの確認ではない**（シリアル
コンソールの出力内容は見ない）。実際にブートしたかどうかは`console.log`を人手で
確認する運用（playgroundでは`scenario.sh`が起動メッセージの有無を確認する、下記
「シリアルコンソールアクセス」参照）。

## UserData注入（cloud-init NoCloud seed disk）

`spec.user_data`が空でないVMには、`internal/compute-agent/vmm/seed.go`（fcvmm/chvmm
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

fcvmm/chvmm共通の仕組み。VMMプロセスの起動直後（`cmd.Start()`成功後）、そのPIDを
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
  ——[Firecracker起動仕様](firecracker-boot.md)「jailer」参照）。
  `driver_hint=CLOUD_HYPERVISOR`には外部jailer(chroot+uid/gid drop)は無いが、
  静的バイナリ(共有ライブラリのchroot問題がそもそも発生しない)+組み込みseccomp
  (既定で有効)という別の形で相応の防御を持つ——
  [cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)参照

## 停止/起動（`Stop`/`Start`）

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

## リサイズ（`Resize`、コールド/ライブ両対応）

`VirtualMachineService.Resize(tenant_id, id, vcpu, memory_mb)`は単一のRPCで
2つの経路に分岐する:`Stopped`のVMはコールドリサイズ、`Running`かつ
`driver_hint=CLOUD_HYPERVISOR`のVMはライブリサイズ。それ以外の組み合わせ
（`Running`＋`FIRECRACKER`を含む）は`FailedPrecondition`で拒否される。
`Stop`/`Start`同様`resource_version`は取らず、Get→フェーズ/driver判定→
mutate→`store.Update`をサービス内で完結させる。

- **コールド経路**（`Stopped`）: `Stopped`中に`spec.vcpu`/`memory_mb`を
  書き換えておけば、次の`Start`が`provisionAndPublish`経由で`vm.Spec`から
  `CreateCommand`を再構築する際に新サイズで起動する——VMMドライバ・
  Reconciler・NATSコマンド、いずれも変更不要
- **ライブ経路**（`Running`＋`CLOUD_HYPERVISOR`）: cloud-hypervisorの
  `--api-socket`（[cloud-hypervisor起動仕様](cloud-hypervisor-boot.md)
  「`--api-socket`」参照）経由で`PUT vm.resize`を呼び、ダウンタイム無しで
  即座に反映する。`internal/compute/liveops.go`の`Reconciler.LiveResize`が
  実装——NATS(`COMPUTE_CMD`、`CmdSubjectHotplug`)でcompute-agentへ
  コマンドを送り、per-request reply-subject（`StreamConsole`と同じ
  パターン）で結果を同期的に受け取ってからgRPCの応答を返す。往復が
  タイムアウト（10秒）すると`Unavailable`
- **Firecrackerがライブ経路を持たない理由**: vCPUスレッドはブート時の
  `/machine-config`で1回固定され、以降VMのライフタイム中変更不可という
  構造的制約（[Firecracker起動仕様](firecracker-boot.md)「spec.vcpuの制約」
  参照）。ライブリサイズは今後もcloud-hypervisor限定のまま
- **Hypervisor容量**: コールド・ライブどちらも、VMが既に割り当て済みの
  Hypervisorに対してのみ`allocated_vcpu`/`allocated_memory_mb`のデルタ調整を
  行う（[VMスケジュール仕様](vm-scheduling.md)「リサイズ時の容量調整」参照）。
  収まらない場合は別のHypervisorへ再スケジュールせず、そのまま
  `ResourceExhausted`で拒否する——`allow_migrate=true`（下記「容量不足時の
  マイグレーションフォールバック」参照）を明示しない限り、ユーザーはVMを
  作り直す以外の手段がない。
  ライブ経路でホットプラグ呼び出し自体が失敗した場合は容量予約を解放し、
  `store.Update`が失敗した場合はベストエフォートで旧サイズへの
  補償的resize-backを試みる（失敗時はログのみ、ゲストと永続化済みspecが
  不整合になりうる既知のギャップ）
- **Quota**: コールド・ライブとも成長方向（vcpu/memory_mbのいずれかが
  増える）のみ判定する。縮小のみのリサイズはquotaを絶対に超過しえないため、
  identityへの問い合わせ自体をスキップする（[Quota仕様](quota.md)
  「強制フロー（VM Resize時）」参照）
- 同一サイズへのResizeは`store.Update`すら呼ばない真のno-op（`resource_version`は
  変化しない）
- **vcpu制約はドライバ依存**: `driver_hint=FIRECRACKER`のVMは、要求されたvcpuが
  Firecracker自身の制約（1または偶数、最大32）を満たさないと`ErrValidation`
  （`InvalidArgument`）で拒否される——VMの`spec.driver_hint`はResizeでは変更できない
  固定値なので、既存VMの`driver_hint`に対して判定する（[Firecracker起動仕様]
  (firecracker-boot.md)「spec.vcpuの制約」参照）。`driver_hint=CLOUD_HYPERVISOR`
  にはこの制約は無い。同じ検証は`Create`にも入っている（`internal/compute/
  virtualmachine.go`の`validateVCPUForDriver`）

### 容量不足時のマイグレーションフォールバック（`allow_migrate`、コールドのみ）

`ResizeVirtualMachineRequest.allow_migrate`（既定`false`）を`true`にすると、
コールドリサイズが現在のHypervisorの容量不足で失敗するケースに限り、
「マイグレーション」（下記）と同じコールド移動を併用して自動的に解決する。
ライブ経路（`Running`＋`CLOUD_HYPERVISOR`）では無視される——ライブリサイズは
マイグレーションしない。

- **常にデフォルトはfalse、明示的なオプトインのみ**: 通常のResizeは同じ
  Hypervisor上でspecを書き換えるだけなので、既にクローン済みのroot disk
  （jail/runディレクトリ）は無傷のまま——`allow_migrate=false`（既定）では
  この性質が保たれる。`allow_migrate=true`で実際に移動が発動した場合のみ、
  「マイグレーション」節と同じ理由でroot diskの中身が失われる。この
  非対称性をユーザーに黙って発生させないため、容量不足時に自動発動させる
  のではなく明示フラグ必須にした（ユーザー確認の上での判断、2026-09-24）
- **失敗する場合にのみ発動**: `grpcserver.Server.Resize`が、まず通常の
  `Service.Resize`（コールド、同一Hypervisor限定）を試し、
  `ErrHypervisorCapacityExceeded`で失敗し、かつ`allow_migrate=true`の場合
  だけ`Reconciler.ResizeWithMigration`を呼ぶ。quota超過・不正なフェーズ・
  無効なvcpuなど他の失敗理由では発動しない（Hypervisorを変えても解決しない
  失敗のため）
- **`ResizeWithMigration`は同期処理**: VMは`Stopped`なので生きている
  プロセスは無く、`Migrate`のような`PhaseMigrating`経由の非同期reconcileは
  不要——Resize自体が元々同期RPC（`Get`→検証→`store.Update`）である契約を
  崩さない。現在のHypervisorを除外した`scheduleVM`の自動選択で**新サイズが
  収まる**Hypervisorを探し（`target_hypervisor`を明示指定するオプションは
  無い——これは配置の意思決定ではなく容量不足のフォールバックのため）、
  見つかった新Hypervisorへ新サイズ分を予約、旧Hypervisorから旧サイズ分を
  解放、`vm.Spec`と`vm.Status.hypervisor`を1回の`Update`で書き換える。
  旧Hypervisorには`Migrate`と同じ`DeleteCommand`をfire-and-forgetで送り、
  残っていたjail/runディレクトリを掃除する
- **新Hypervisorでも収まらなければ**`ErrHypervisorCapacityExceeded`
  （`ResourceExhausted`）——`ErrUnschedulable`ではなく、通常のResize失敗と
  同じエラーにそろえてある
- **CLI**: `kyuusha vm resize -id=... -vcpu=... -memory-mb=... -allow-migrate`

## マイグレーション（`Migrate`、コールドのみ）

`VirtualMachineService.Migrate(tenant_id, id, target_hypervisor)`はVMを別の
Hypervisorへ移す。`Stopped`のVMのみ受け付ける（`FailedPrecondition`）——
ライブマイグレーションは存在しない（`docs/architecture.md`「ライブ
マイグレーション不要」という設計原則そのままで、対応する予定もない）。

**引き継がれるもの・引き継がれないものが非対称**な点が最大の注意点:

- **引き継がれる**: `NetworkInterface`（IP/MAC）と`VolumeAttachment`
  （Volumeの実データ）。どちらも元々Hypervisor非依存の参照モデルなので、
  同じ`iface-<vm-id>-*`/`volattach-<vm-id>-*`という決定的な名前のまま
  新Hypervisor上で再利用される（`provisionAndPublish`の
  `createNetworkInterfaces`/`createVolumeAttachments`が名前ベースで
  冪等なことがそのまま効く）
- **引き継がれない**: root disk（`KERNEL_ROOTFS`のrootfs/`QCOW2`のdisk）
  の中身。kyuushaのroot diskは各Hypervisorのローカルディスク上でImageから
  都度クローンされる、意図的にエフェメラルな設計（Hypervisor間のディスク
  転送パス自体が存在しない）なので、移行先では同じImageから作り直される
  ——ゲストが起動後にroot diskへ書いた差分は失われる。永続化したいデータは
  元々Volumeに置く設計（`docs/architecture.md`のpet/cattle区別廃止と同じ
  前提）なので、この非対称は設計上の欠陥ではなく「rootディスクは
  cattle、Volumeだけがpet」という一貫した扱い

**フロー**（`internal/compute/reconciler.go`の`migrateVM`、`PhaseMigrating`）:

1. `Migrate`は意図を記録するだけ（`Status.Phase = Migrating`、
   `Status.MigrateTarget = target_hypervisor`）で即座に返る——`Stop`/`Start`
   と同じ「Service側はGet→mutate→Updateのみ、実際の処理はreconcile()」
   という設計
2. `migrateVM`が移行先Hypervisorをスケジュールする
   （`hypervisor_service.go`の`scheduleMigration`）:
   - `target_hypervisor`が空なら、`scheduleVM`の通常のフィルタ+Pick戦略で
     自動選択する。ただし現在のHypervisorは候補から除外——除外しないと
     「移行の結果、元の場所に戻ってくる」という無意味な成功がありうるため
   - `target_hypervisor`が指定されていれば、そのHypervisor**だけ**を対象に
     同じハードフィルタ（Ready/schedulable/driver対応/zone/容量）で検証する
     ——指定を無条件に信用せず、フィルタを満たさなければ
     `ResourceExhausted`/`FailedPrecondition`相当で拒否する。現在の
     Hypervisorと同じIDを指定した場合は`InvalidArgument`
   - どちらの経路も失敗時はVMを`Migrating`のまま留め、`Unmigratable`
     Conditionを記録する。`runRetrySweep`が`PhasePending`と全く同じ
     「無条件で毎tick再試行」を`PhaseMigrating`にも適用する——容量が空くのは
     Hypervisor側の変化であり、このVM自身のWatchイベントには現れないため
3. 成功したら、元Hypervisorの容量予約を解放し、`DeleteCommand`を元
   Hypervisorへfire-and-forgetで送る（`Migrate`はStoppedのVMにしか効かない
   ので生きているプロセスは無く、元Hypervisor上に残っていた
   jail/runディレクトリ——古いroot diskの実体——を掃除するだけ。実質
   「削除」と同じ処理を、VMリソース自体は消さずに1ホスト分だけ行う）
4. `Status.Hypervisor`を新Hypervisorへ書き換え、`Phase = Scheduled`へ
   遷移させる——ここから先は新規Createと全く同じ`provisionAndPublish`
   （`case PhaseScheduled, PhaseStarting:`と合流）が、新Hypervisor上で
   kernel/rootfsの取得・tap配線・Volume発見をやり直す

**CLI**: `kyuusha vm migrate -tenant=... -id=... [-target-hypervisor=...]`。
`-target-hypervisor`省略時は自動選択。

playgroundで実機確認済み: Stopped状態のVMを`vm migrate`（自動選択）で
別Hypervisorへ移し、`Migrating`→`Provisioning`→`Running`と遷移して実際に
Firecrackerゲストが新Hypervisor上で起動し、`NetworkInterface`のIP/MACが
移行前と完全に同一のまま(`kyuusha vm console`でゲスト自身が同じIPを
設定するログまで確認)、かつ元Hypervisor側のjail/runディレクトリが
掃除されていることを確認した。

## Volume attach/detach（`AttachVolume`/`DetachVolume`、コールド/ライブ両対応）

`Resize`と全く同じ分岐パターン——`Stopped`のVMはコールド、`Running`＋
`CLOUD_HYPERVISOR`のVMはライブ、それ以外は`FailedPrecondition`。詳細は
[Volume仕様](volume.md)「Volume attach/detach」参照。要点のみ：

- **コールド経路**: `AttachVolume`は`vm.Spec.Volumes`へ追加するだけ（実際の
  VolumeAttachmentは次の`Start`まで作らない）。`DetachVolume`は非対称に
  実体のVolumeAttachmentも即座に削除する（排他ロックを握ったまま放置しないため）
- **ライブ経路**: `internal/compute/liveops.go`の`Reconciler.LiveAttachVolume`/
  `LiveDetachVolume`が実装。Attach側は**先にVolumeAttachmentを作成し
  Attachedになるまでポーリングしてから**cloud-hypervisorへ`PUT vm.add-disk`
  （順序が逆だと、block-storageの排他ロックが無いまま別VMが同じVolumeへ
  同時attachしうる）。Detach側は逆に**先に`PUT vm.remove-device`でゲストから
  外してから**VolumeAttachmentを削除する（ロックを握ったまま外すとデータ破損
  リスクがあるため）。どちらもNATS往復は`Resize`と同じ`CmdSubjectHotplug`/
  reply-subjectパターンを共有する
- quotaチェック・Hypervisor容量予約は無い——既存Volumeのattach/detachはどちらにも
  影響しない
- `createVolumeAttachments`（compute/volume.go）の子リソース命名は
  `volattach-<vm_id>-<volume_id>`（VolumeIDベース）——ライブ経路もこの同じ
  ヘルパーを再利用する（Volume仕様参照）

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

ドライバ固有の要件（Firecrackerバイナリの取得、cloud-hypervisorバイナリの取得等）は
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
  （`compute-agent-1/2/3`の`command:`）。playgroundは
  `-drivers=FIRECRACKER,CLOUD_HYPERVISOR`——これが無い（既定値`FIRECRACKER`のみ）と、
  `driver_hint=CLOUD_HYPERVISOR`のVMはスケジュール可能なHypervisorが1台も無い状態に
  なり、`Pending`のまま進まなくなる（[VMスケジュール仕様](vm-scheduling.md)
  参照。ライブ検証で実際に踏んだ実バグ）
- kernel/rootfsは`image-assets`という専用compose serviceが配信する（プレーンHTTP、ホストには
  公開しない）。ドライバを問わず同じアセットを共有する——[Firecracker起動仕様](firecracker-boot.md)
  「playgroundでの構成」参照

## この実装がカバーしないもの（共通）

- `user_data`の機密情報対応（保存時暗号化、監査ログからの除外。
  `docs/architecture.md`「UserData注入」の「機密情報の扱いに関する注記」参照）
- 本物のcloud-initを動かすゲストでの動作確認（playgroundの最小Alpineゲストには
  cloud-init自体が入っていないため、seed diskが正しく届くことまでしか確認していない）
- **リサイズ時のHypervisor間移行**: 新サイズが現在のHypervisorの空き容量に
  収まらない場合、別のHypervisorへVMを移動してリサイズを成立させる機能は無い
  （上記「リサイズ」節参照、拒否のみ）
- **イメージローカルキャッシュのエビクション**: `internal/compute-agent/imagestore`は
  取得したdigestを際限なく保持し続ける——LRU等の削除ロジックがまだ無い
  （[docs/open-questions.md](../open-questions.md)「イメージのローカル管理」参照）

プロセス隔離もStop/Start（一時停止/再開）もResize（コールド/ライブ）もVolume
attach/detach（コールド/ライブ）ももう「共通の未実装事項」ではない——jailer相当の隔離は
`driver_hint=FIRECRACKER`が実jailer(chroot+uid/gid drop)、`driver_hint=CLOUD_HYPERVISOR`
が静的バイナリ+組み込みseccompという別の形で、それぞれ対応済み（上記「cgroupリソース
制限」節参照）。Stop/Start・`Resize`・`AttachVolume`/`DetachVolume`はそれぞれ上記
「停止/起動」「リサイズ」「Volume attach/detach」節参照。ドライバ固有の未実装事項
（例: Firecrackerのクロスhypervisorネットワーク疎通、cloud-hypervisorのPCI
passthrough/vhost-user）はそれぞれの仕様書の「この実装がカバーしないもの」を参照。
