# 実装フェーズの迷い事項

docs/architecture.md の「未決事項」は設計レベルの論点用。こちらは実装を進める中で
出てきた、まだ判断を保留している細かい話を随時追加していくメモ。

## CLIの `-tenant` フラグをトークンのクレームからデフォルトすべきか（解決済み・実装済み、2026-09-19）

`kyuusha vm`/`kyuusha tenant` の各サブコマンドは `-tenant`/`-id` を常に明示指定させていた。
非adminにとっては認可上どうせ自分自身の `tenant_id` としか一致しないため実質冗長だが、
adminは「どのテナントを操作するか」を明示する必要があり、トークンのクレームから
自動導出する設計にはできない（意味が壊れる）。

CLI側だけの利便性機能として実装した: `-tenant` 省略時は`-token`（または
`$KYUUSHA_TOKEN`）のJWTペイロードを署名検証なしでローカルデコードし、`tenant_id`
クレームをデフォルト値にする（`resolveTenant`、`cmd/kyuusha/main.go`）。`role`
クレームが空でないトークン（`admin`/`storage-admin`/`network-admin`/`viewer`）は
自動補完せず明示指定を要求する——冒頭の懸念通り、これらのロールでは`tenant_id`が
「操作対象のテナント」を意味しないため。ワイヤプロトコル・サーバー側authzモデルには
一切影響しない。詳細は[CLI仕様](specs/cli.md)「共通」参照。

## Hypervisor自己登録の認証（解決済み: zone検証+軽量な個体識別/失効を実装、本格PKIは不採用）

2026-09に、東西通信全体のmTLS化（`internal/mtls`）と、`RegisterHypervisor`の
zoneスコープ付きbootstrapトークン検証（`internal/bootstraptoken`）を実装した。
元の設計の「identityが軽量CAを兼ねてハイパーバイザー専用のmTLS証明書を動的発行する」
という本格的なPKIは、2026-09-11に改めて検討した上で不採用と確定した——秘密鍵の発行・
保存・失効まで面倒を見る運用は、kyuusha全体の「運用コストの重い自前実装・既製品を
避ける」路線（libvirt見送り・Ceph不採用と同じ理由）と合わない。

代わりに、bootstrapトークンの仕組みだけを軽く拡張して「個々のハイパーバイザーの識別」
と「失効」を実現した（2026-09-11、詳細は
[Hypervisor登録・死活監視仕様](specs/hypervisor-bootstrap.md)「個体識別と失効」参照）:
トークンに任意で`hypervisor_id`クレームを持たせられるようにし（`-hypervisor=<id>`）、
`Register`時にリクエストの`hypervisor`フィールドと一致するか確認する。失効は
`HypervisorSpec.revoked`（`SetRevoked` RPC）で、そのIDでの以後の`Register`を一律拒否する。

**割り切っている点（ユーザー確認済み）**: これは将来の`Register`呼び出しを拒否する
だけで、すでに確立している東西通信（heartbeat等）をその場で強制切断することはできない
——ハイパーバイザー専用のmTLS証明書が無い以上、個別のセッションを識別して止める手段が
無いため。悪意あるハイパーバイザーの存在を強く警戒するシナリオは今のkyuushaの脅威モデルの
優先度としては低いという判断で、この割り切りを受け入れた。

残っているのは主に: bootstrapトークンの使い捨て化（同じトークンを同じハイパーバイザーの
再起動のたびに繰り返し使うこと自体は妨げていない）。必要になった時点で着手する。

## cloud-hypervisorドライバのブート可能ディスク対応（Windows等の非Linuxゲスト、2026-09-15実装済み）

2026-09に`driver_hint=QEMU`として実装し、同月中にcloud-hypervisorへ置き換えた
（`internal/compute-agent/chvmm`、[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)
参照）。当初はFirecrackerと同じ「kernel/rootfsを直接指定する」方式のみで、
QEMU/cloud-hypervisorが本来可能な「ブートローダー内蔵の自己完結ディスク」
（`QCOW2`）経由の起動は実装していなかった。

2026-09-15、`chvmm`にUEFIブート（edk2の`CLOUDHV.fd`ファームウェア＋
`--disk path=...,image_type=qcow2`、rawへの変換はしない）を追加し解消した
——[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)「起動方式2:
UEFIブート」参照。Alpine Linux公式cloud image（無改変）での実機ブートを
ログインプロンプト到達まで確認済み。fcvmm（Firecracker）は構造的に対応不可の
ままで、`internal/compute/image.go`のCreate時バリデーションが`QCOW2`形式に
`driver_hint=CLOUD_HYPERVISOR`を強制する。

**残る未確認事項**: Windows自体での実機ブートはまだ確認していない（UEFI機構
そのものはAlpine cloud imageで実証済み）。

## block-storageバックエンドの責務境界の作り直し（解決済み・実装済み、2026-09-10）

2026-09-10、`docs/architecture.md`「block-storageのバックエンド抽象化」節を、
「厩舎がプロビジョニング＋export＋接続まで全部やる」という当初の`StorageBackend`
設計から、「プロビジョニングとHypervisor単位の接続確立（iSCSI/NVMe-oFログイン、
NFSマウント）は運用者側の責務、厩舎は参照メタデータ＋排他制御＋スケジューリング
制約＋起動時のデバイス/ファイル発見のみを持つ」という縮小した境界へ書き換え、
実装した（commit `92d359b`。`storage-agent`・`internal/compute-agent/iscsi`は
完全に削除、新しい`internal/compute-agent/volumeref`が発見ロジックのみ担う）。
iSCSI/NVMe-oF/NFSは「Hypervisor単位の事前接続＋接続済みセッション内の1リソースを
参照するだけ」という統一モデルに収まった。`playground/scenario.sh`で実VM経由の
永続化を実際に確認済み（詳細は`docs/specs/volume.md`参照）。

## Hypervisorのストレージ接続自己申告を動的化すべきか（静的で実装中、将来の検討事項）

`StorageConnection`実装（上記）は当面、Hypervisorの`storage_connections`自己申告を
起動時1回きりの静的な宣言として扱う。将来的に、heartbeatのたびに実際に
到達性を再確認してから報告する動的な方式に変える価値があるか、という
オープンクエスチョン。

- **静的（今の実装）**: シンプル。ただし接続が後から切れても検知できず、
  一度`Ready`になった`StorageConnection`/`Volume`の状態が陳腐化しうる
- **動的**: heartbeatごとに再確認するので状態を正直に保てる。ただしNFSが
  ハングした場合の`stat()`呼び出しが返ってこず、heartbeatループ全体を
  詰まらせるリスクがある（別goroutine+タイムアウトでの隔離が要る等、
  実装コストが上がる）

`StorageConnection`の実装完了後、運用実績を見てから判断する。この節は
`StorageConnection`関連の他の節と違い、実装が終わっても削除しない
（恒常的な将来課題として残す）。

## Volumeの申告内容（存在確認・サイズ）が一切検証されない（解決済み、下記参照）

もともとの問題: `CreateVolume`が`protocol`/`storage_connection`/`identifier`の
形式チェックのみで即座に`Ready`にしていたため、到達可能性も`size_gb`（完全な
自己申告、`max_volume_gb`のQuota強制が事実上の申告制になっていた）も一切検証
されなかった。2段階で解決済み:

1. **サイズの検知**（2026-09-11、commit `30409fb`）: VM起動時、
   `internal/compute-agent/volumeref.Resolve`が見つけたパスの実サイズを取得し、
   宣言`size_gb`と10%以上食い違えば`vmm.WarnIfSizeMismatch`が構造化ログで警告する
   （block-storage/proto側の変更は伴わない、ログベースの検知のみ）
2. **存在確認**（2026-09-11、`StorageConnection`リソース＋非同期検証フロー実装）:
   `Volume`はもう即`Ready`にならず、`StorageConnection`が`Ready`（宣言した全ゾーンで
   到達確認済み）かつ`identifier`の実在がcompute-agent経由で確認できて初めて
   `Ready`になる。詳細は`docs/architecture.md`「追記（2026-09-11）」と
   `docs/specs/volume.md`「検証フロー」参照

**サイズ食い違いは検知だけでなく自動補正するところまで実装済み（2026-09-11、下記参照）**:
`VerifyVolumeResult`の`size_bytes`（元々存在していたのに捨てられていた）を
`handleVerifyResult`で`spec.size_gb`と比較し、10%以上ずれていれば
`correctDeclaredSize`が**`spec.size_gb`自体を実測値へ書き換え、`tenant_usage`も
その差分だけ調整する**（単に`Volume.status.conditions`へ`SizeMatchesDeclaration`
として記録するだけの案から発展させた）。ストレージ管理者が手入力する`size_gb`は
入力ミスが起きやすく、実測値が分かった時点でQuota会計をズレたままにしておく理由が
無いという判断——`Volume.spec.size_gb`は元々「Create時は自己申告、検証できない」
フィールドだったが、検証できるようになった以上は直すべき、という整理。補正で
テナントのQuotaを超えてしまっても、補正自体は適用した上で`conditions`の
`QuotaExceededAfterCorrection`で見えるようにするだけに留め、既存Volumeの破棄・
detachはしない（すでに動いている可能性のあるリソースを事後的なQuota判断で
壊さないという、他の場面と同じ判断）。詳細は`docs/specs/volume.md`「検証フロー」
参照。`vmm.WarnIfSizeMismatch`（VM起動時のログ警告）はそのまま残す——こちらは
Volume検証と独立にVM起動のたびに実際のアタッチ経路で発生するので、両方に意味がある

**`device_path`/`status.hypervisor`（`VolumeAttachment`側）のAPI越しの可視化も
解決済み（2026-09-11）**: compute-agentがVM起動時に実際に解決したVolumeの情報を
`ms.blockstorage.evt.<hypervisor>.volume.attached`でblock-storageへ報告するように
なった。詳細は`docs/specs/volume.md`「device_path/hypervisorの報告」参照

**まだ解決されないまま残る部分**（別タスク）:
- VM起動が寛容に劣化しない件（`volumeref.Resolve`が失敗するとVM起動全体が失敗する）
  自体は変更していない——永続データを積むVolumeを黙って外すより失敗を明示する方が
  安全という判断で、"バグ"ではなくトレードオフとして残している
- 検証コマンドのリトライ間隔・上限（現状は無期限、10秒ごと）は未チューニング
- Volume検証コマンドのリトライ間隔・上限（現状は無期限、10秒ごと）は未チューニング

## QEMU用のjailer相当の隔離方式（2026-09-12、ドライバ置き換えにより解消）

2026-09に`driver_hint=FIRECRACKER`のVMをjailer（chroot + uid/gid権限降格）でラップした
（[Firecracker起動仕様](specs/firecracker-boot.md)「jailer」）。jailer自体はFirecracker
専用ツールでQEMUをラップできないため、`driver_hint=QEMU`にはまだ同等の隔離が無い、
という問題が長らく残っていた——一度は「別プロジェクトとして本格的な汎用jailerを
立ち上げる」方針（[QEMU jailerハンドオフ](qemu-jailer-handoff.md)）、次に「既製の
minijail/nsjailをkyuusha本体へ直接統合する」方針（[QEMU jailer設計](specs/qemu-jailer.md)）
と2度方針転換したが、**2026-09-12、そもそも実QEMUを使うのをやめてcloud-hypervisorへ
置き換えたことで、この問題自体が解消した**: cloud-hypervisorは静的バイナリ
（共有ライブラリのchroot問題が発生しない）で、seccompを内蔵しており、外部jailerが
実質不要になった。

詳細な経緯（QEMUの動的ライブラリ依存の発見、libvirt/自前jailer/別プロジェクト/
既製jailer統合という検討の変遷）は[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)
「QEMUからcloud-hypervisorへの置き換え」、および上記2つの旧ドキュメント（どちらも
冒頭に撤回済みの注記あり、経緯の記録として残している）を参照。

## イメージのローカル管理/OCIレジストリ対応: Track 1・2実装済み、残る未決事項（2026-09-14）

`docs/architecture.md`「イメージのローカル管理」Track 1
（`internal/compute-agent/imagestore`、digest検証付きの共有キャッシュ＋
reflink CoWコピー、containerd自体は輸入せず自前実装）と、Track 2
（`oras-go/v2`によるOCIレジストリ参照対応、`internal/image`のOCI到達性チェック、
playgroundの`registry`サービス＋`ocitool`によるseed）の両方を実装し、
playgroundで実VM起動まで確認済み（HTTP経由・OCI経由どちらも同じ
`blobs/sha256/<hex>`パスに同じdigestでキャッシュされ、実Firecracker起動・
ゲストブート成功）。overlayfs以外のフォールバックは`CloneFile`が`FICLONE`
失敗時に通常コピーへ自動フォールバックする形で実装時に解決済み。残る未決事項:

- **キャッシュのエビクション（LRU等）が未実装**: `imagestore.Store`は
  取得したdigestを際限なく保持し続ける——削除するロジックがまだ無い
  （旧ドキュメントが挙げていた「containerdのGCモデルとの整合」という論点自体は
  containerdを使わなくなったため消えたが、「いつ・何を削除するか」を自前で
  設計する必要は残っている）
- **`kyuusha image build`のカーネル自動選択は未実装**（2026-09-15、本体は実装済み）:
  `cmd/kyuusha/imagebuild.go`としてDockerfileからのrootfsビルド・push・Image
  Create一気通貫のパイプラインは実装・実機確認済み（[Image仕様](specs/image.md)
  「`kyuusha image build`」参照）。残っているのは「kyuushaが用意する少数の推奨
  カーネルから自動選択」（`docs/architecture.md`「イメージ作成体験」の当初構想）
  のみ——現状は`-kernel-url`/`-kernel-digest`の明示指定が必須
- **Track 3（Dragonfly/Spegel等のP2P導入）着手のタイミング判断基準**: Track 2
  完了により技術的な前提（レジストリプロトコル経由の配布）は揃ったが、実際に
  P2Pを導入するかは別判断。「Dragonfly級のP2Pが実際に必要なスケールに達した」と
  何をもって判断するか（例: playground規模を超えた実クラスタでのorigin負荷の
  実測、特定テナントからのバルクVM作成要求の頻度、等）。今のところ具体的な
  閾値は未定義
- **既存Image（`digest`が空のもの）の扱い**: `imagestore`は`digest`が空なら
  未検証の旧キャッシュパスへフォールバックする後方互換動作を持つが、
  `internal/image`のCreate時バリデーションを将来`digest`必須にするかどうかは
  未検討（今回は意図的に見送った——「変更しないもの」参照）

## ロールベースの細かい認可（RPCメソッド・リソース種別単位）をやるべきか

現状`role`クレームは実質`admin`かそれ以外かの2値でしか使われておらず、「このロールは
CreateはできるがDeleteはできない」のようなメソッド単位・リソース種別単位の制御はない
（[認証・認可仕様](specs/authn-authz.md)参照）。加えて、`internal/authz`のinterceptorは
そもそもOPAへRPCメソッド名を渡していない（`tenant_id`/`role`のみ）ため、ポリシーを
書き足すだけでは足りずinterceptor自体の拡張も要る。

docs/architecture.mdでは「主要な外部クライアントはKaaSコントローラー1つで、自クラスタの
全リソース種別を管理する必要があるため分割の実利が薄い」として意図的に据え置いていた
（OPA採用によりいつでも先送りできる、という前提込みで）。判断保留中。

## cloud-hypervisor限定のライブホットプラグ（vcpu/memory resize + Volume attach）（見送り中、詳細設計は確定済み）

`VirtualMachineService.Resize`（2026-09-19実装）も`AttachVolume`/`DetachVolume`
（同じく2026-09-19実装）も、どちらも`Stopped`のVMに対するコールド操作のみ——
実行中VMのvcpu/memory_mbやアタッチ済みVolumeをダウンタイム無しで変更する手段は
まだ無い（[VirtualMachine仕様](specs/virtual-machine.md)「リサイズ」「Volume
attach/detach」、[Volume仕様](specs/volume.md)「Volume attach/detach」参照）。
当面このままにする、という判断（見送り、まだ着手しない）。

### ドライバごとの実現可能性（両方とも同じ非対称性）

- **cloud-hypervisor**: vcpu/memoryは元々「boot-vcpus/max-vcpus」の分離設計を持ち、
  本物のvcpuホットプラグ＋`virtio-mem`によるメモリホットプラグに対応した
  `--api-socket`経由のresize API（`PUT /api/v1/vm.resize`）を備えている。ディスクは
  さらに単純で、`PUT /api/v1/vm.add-disk`/`vm.remove-device`が事前にスロットを
  宣言する必要すら無い完全動的なPCIホットプラグに対応している。このリポジトリは
  現状APIソケットを一切使わずCLIフラグで起動時に一括設定するだけなので
  （[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)参照）、やるとすれば
  APIソケット導入がまず必要（vcpu/memoryリサイズだけは起動時に`--cpus boot=N,max=M`
  等のホットプラグ余地宣言も要る——ディスクhotplugはこれが不要）
- **Firecracker**: vCPUホットプラグは構造的に不可能——vCPUスレッドはブート時に
  `/machine-config`で1回設定され、以降VMのライフタイム中1:1で固定
  （[Firecracker起動仕様](specs/firecracker-boot.md)「spec.vcpuの制約」参照）。
  `virtio-balloon`によるメモリの縮小/再拡大は実行中でも可能だが、ブート時に設定した
  `mem_size_mib`という上限を超えて増やすことはできず、真の意味でのメモリリサイズ
  ではない。ディスクも同様に、Firecrackerの`PATCH /drives/{id}`は事前宣言済みの
  スロットのbacking fileを後から差し替えるだけで、新しいスロットを動的追加する
  経路自体が無い（cloud-hypervisorのadd-diskと違い、上限個数を起動時に決め打ちする
  必要がある）

やるとしたらどちらもcloud-hypervisor限定の機能になる。ダウンタイムを許容できない
ワークロードが実際に出てくるまでは、コールドリサイズ/コールドattach・detachで
大半のユースケースはカバーできる、という判断で見送っている。

### 詳細設計（2026-09-19、着手時にゼロから再設計しなくて済むよう記録）

大きな判断はユーザー確認済み：
- Resize/AttachVolume/DetachVolumeとも**単一RPCでphase分岐**（コールド=Stopped、
  ライブ=Running+CLOUD_HYPERVISOR）——別RPCに分けない
- ホットプラグ対応起動引数（`--cpus boot=N,max=M`、memory hotplug region）は
  **全CLOUD_HYPERVISOR VMに無条件で付与**（コスト実質ゼロ、spec変更不要）

設計の骨子：
- `internal/compute-agent/chapi`（新規）: cloud-hypervisorの`--api-socket`に対する
  Unixソケット越しHTTPクライアント（`add-disk`/`remove-device`/`vm.resize`の3メソッド
  のみ、標準ライブラリのみで十分——go.mod未使用のサードパーティ依存は無し）
- `internal/compute-agent/vmm.Hotplugger`という**オプショナル**interfaceをchvmmだけが
  実装（fcvmmはスタブ不要）。`chvmm.Manager`の`runningVM`にAPIソケットパスを持たせる
  必要は無い——`ConsoleLogPath`と同じく`vmID`の純粋関数として毎回計算できる
  （`BootRecord`のスキーマ変更も不要）
- `compute.Service`は`LiveOps`という小さなinterfaceを持ち、`*Reconciler`がその実装を
  担う（`StreamConsole`が`*Reconciler`メソッドとしてNATS inbox往復している既存パターンと
  同じ——`Service`自体はNATSハンドル(`nc`/`js`)を持たないため）
- ライブ操作はJetStream経由でコマンドを送り（`COMPUTE_CMD`、`StopCommand`と同じ
  at-least-once/ack-on-receipt契約）、結果は`StreamConsole`と同じくper-request inbox
  （コマンド自体が`reply_subject`を持つ）で同期的に受け取る——`COMPUTE_EVT`は使わない
  （結果に興味がある相手はRPC呼び出し元1人だけなので、durable/replayableである必要が
  無い）。RPCは呼び出し元でブロックし、成功/失敗を直接gRPCエラーとして返す
  （Create/Stopのような非同期poll型にはしない）
- ライブResizeのHypervisor容量予約は既存の`resizeHypervisorCapacity`/
  `releaseHypervisorCapacity`をそのまま再利用——NATS往復が容量予約とspec更新の間に
  挟まる分、ロールバック分岐が1つ増えるだけ
- ホスト側cgroup更新は`internal/compute-agent/cgroup.Apply`をそのまま再利用
  （既に冪等——`cpu.max`/`memory.max`を書き換えて同じpidを`cgroup.procs`へ再登録
  するだけ）
- block-storageの`tryAttach`（`internal/block-storage/service.go`）は変更しない——
  ライブ配線は`Attached`到達後にcomputeが上乗せする別ステップとして設計し、
  block-storageにVMM知識を持ち込まない。VolumeAttachmentへの`device_path`報告も
  既存の`VolumeAttachedEvent`/`handleVolumeAttached`（fire-and-forget）をそのまま
  流用できる——起動時attachとライブattachで同じ経路
- playground検証には`ch-remote`のcompute-agentイメージへの追加が要る（実際にPCI
  デバイスが増えたこと/vcpuが変わったことを、kyuusha自身のコードとは独立したツールで
  確認するため）
