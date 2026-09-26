# リリースノート

このプロジェクトの「いつ・何が変わったか」の記録。**設計判断の「なぜ」は
[docs/architecture.md](architecture.md)、現状の仕様は[docs/specs/](specs/README.md)を
参照**——ここには日付付きの事実のみを置き、設計トレードオフの深掘りはarchitecture.mdへ
リンクする形にする。

## 2026-09-26

- スケジューラにVolume容量ではなく**storage_connectionによるフィルタ**を追加
  （`docs/specs/volume.md`「スケジューリング時のフィルタリング」が長らく
  未実装として残していた項目）。VMが要求するVolumeの`storage_connection`を
  全て自己申告済みのHypervisorだけが候補に残る——`internal/compute/
  hypervisor_service.go`の`filterSchedulable`/`scheduleVM`/`scheduleMigration`
  を`scheduleConstraints`構造体（`Zone`/`StorageConnections`/`Exclude`）へ
  リファクタし、`validateVolumes`がzoneの導出と同じ仕組みで
  `storage_connection`一覧を返すように変更。Create時の初回スケジュール
  だけでなく、`Migrate`・`Resize`の容量不足フォールバックの再スケジュールも
  同じフィルタを通る。playgroundで実機確認済み: あるHypervisorだけに
  特定の`storage_connection`を宣言させ、空き容量では別のHypervisorが
  選ばれるはずの状況でも、正しくその接続を持つHypervisorへスケジュール
  され、Volumeが実際にAttachedまで到達することを確認
  （[VMスケジュール仕様](specs/vm-scheduling.md)「フィルタ（ハード制約）」参照）

- PCI/GPUパススルーのkyuusha側実装を追加（`docs/architecture.md`「PCIデバイス
  (GPU等)パススルー」節が長らく設計の型だけでTODOとしていた項目）。
  `RegisterHypervisorRequest.available_devices`フィールドを新設し、
  compute-agentの新しい`-pci-devices`フラグ（`pci_address:vendor_id:device_id`
  のカンマ区切り、宣言された各アドレスが実際に`vfio-pci`に束縛されているか
  `/sys/bus/pci/devices/<addr>/driver`で検証してから自己申告）で
  `Hypervisor.status.available_devices`に反映されるようにした。スケジューラの
  `filterSchedulable`/`scheduleVM`/`scheduleMigration`に`spec.pci_devices`
  （`vendor_id`/`device_id`/`count`）のフィルタと排他予約
  （`reservePciDevices`/`releasePciDevices`/`restorePciDevices`）を追加し、
  `chvmm`がcloud-hypervisor起動時に`--device path=/sys/bus/pci/devices/<addr>/,
  iommu=on`として反映するところまで配線した。テナント単位の統制として
  `Tenant.spec.quota.pci_devices`（`(vendor_id, device_id)`ごとの数量上限、
  リストに無い組は上限0の明示許可制）もCreate時のOPA判定に追加。
  `driver_hint`が`CLOUD_HYPERVISOR`以外のVMに`spec.pci_devices`を指定した
  場合は`validatePciDevicesForDriver`がCreate時に拒否する。
  **未検証**: このホストはBIOS/UEFI側でVT-d(IOMMU)が無効（DMARテーブル自体が
  存在しない）であることが判明し、物理的なBIOSアクセスが必要なため、実機での
  実際のVFIOパススルー動作は今回未確認——スケジューリング/予約/解放ロジックの
  ユニットテストとcloud-hypervisor起動引数の構築までを実装範囲とし、実機検証は
  BIOSでVT-dを有効化できる環境が整い次第の課題として残す
  （[VirtualMachine仕様](specs/virtual-machine.md)「PCIデバイスパススルー」、
  [Quota仕様](specs/quota.md)参照）

## 2026-09-25

- `VirtualMachineService.Create`向けのAdmission Webhook（Kubernetesの
  `ValidatingAdmissionWebhook`相当）を実装。新規`internal/admissionwebhook`
  パッケージ（リソース非依存、HTTP POST+JSON、gRPCではない）を`compute`サービス
  起動時の`-admission-webhook-urls`（カンマ区切り、複数指定可）で有効化。
  Image/NetworkInterface/Quotaの内部バリデーションを全て通した後・実際に
  永続化する前の最後のゲートとして呼ばれ、設定した全URLが`allowed:true`を
  返して初めて許可する（1つでも拒否すれば全体を拒否）。webhookが疎通不能な
  場合の挙動は`-admission-webhook-fail-open`で選択可能（既定fail-closed）。
  webhook URLの一覧はサービス起動時のオペレータ設定のみで、APIからテナントが
  登録する経路は作らない——`internal/compute-agent/netsetup`のVNAPプラグイン
  と同じセキュリティ上の割り切り。playgroundで実機確認済み（許可/拒否双方の
  応答、webhook疎通不能時のfail-closed拒否、拒否されたCreateがVMを一切
  永続化しないことを確認。[外部システム連携仕様](specs/external-integration.md)
  「ゲート系(作成側): Admission Webhook」参照）

- `docs/network-deployment-guide.md`に、EVPN Type-5（pure L3）デプロイ向けの
  「3.5. Type-5デプロイの場合」節を追加。`vlan_id`は厩舎自身のローカルな帳簿番号
  （Hypervisor上のブリッジ/ルーティング分離キー）であり、ワイヤ上の本物の802.1Qタグ
  であることを強制されない点、「1 VLAN = 1 VRF」という既定マッピングがType-5には
  適用されない点、Route Distinguisherを`vlan_id`単体から機械的に導出してはいけない点
  （AZ内でのみ一意なため）を明記。あわせて`examples/vnap-plugins/frr-type5.sh`
  （VNAPのサンプル実装）を追加——共有ブリッジを使わず、VMごとのtapへ`gateway_ip`を
  `/32`で直接付与しproxy ARPを有効化した上で、VM自身のIPを`/32`のホストルートとして
  カーネルとFRR（`vtysh`経由）へ注入する。playgroundで実機確認済み（[network仕様]
  (specs/network.md)「VNAP（ローカルなtap配線プラグイン契約）」参照）

- `internal/compute-agent/netsetup`に、tap配線のローカルなスイッチattach/detach
  ステップを外部バイナリへ委譲できるVNAP（VM Network Attach Protocol）契約を実装。
  compute-agentの`-network-attach-bin`で指定、未指定なら既存の固定Linuxブリッジ
  実装のまま。tapデバイス自体の作成・削除は常に厩舎が担い、プラグインは
  「作成済みのtapをローカルスイッチへattach/detachする」ことだけを担当する
  （CNI互換ではなく、バイナリ+stdin JSON+exit codeという呼び出し規約パターンだけを
  参考にしたkyuusha独自の契約——`docs/architecture.md`「VMのネットワーク接続を
  CNIのようにプラガブルにすべきか」参照）。playgroundで実機確認: 既定のLinux
  ブリッジ経路は無変更のまま実VM起動を確認、外部プラグイン経由でも正しいattach/
  detach JSONペイロードを受け取りゲストが正常に起動、プラグイン失敗時もtap自体は
  必ず削除されることを確認（[network仕様](specs/network.md)「VNAP（ローカルな
  tap配線プラグイン契約）」参照）

## 2026-09-24

- `VirtualMachineService.Resize`のコールド経路に、容量不足時のマイグレーション
  フォールバックを追加（`ResizeVirtualMachineRequest.allow_migrate`、既定
  `false`）。現在のHypervisorに新サイズが収まらない場合、`allow_migrate=true`を
  明示した時だけ`Migrate`と同じコールド移動を併用して収まる別Hypervisorへ
  移す（root diskはそのケースだけImageから作り直され中身が失われる——既定
  falseのままなら従来通りroot diskは無傷でResourceExhaustedのまま）。
  `Reconciler.ResizeWithMigration`として実装、`grpcserver.Server.Resize`が
  通常のコールドResizeが`ErrHypervisorCapacityExceeded`で失敗した場合のみ
  フォールバックとして呼ぶ。CLIは`kyuusha vm resize -allow-migrate`
  （[VirtualMachine仕様](specs/virtual-machine.md)「容量不足時のマイグレーション
  フォールバック」参照）

## 2026-09-23

- `VirtualMachineService.Migrate`を実装。`Stopped`のVMを別Hypervisorへ移す
  コールドマイグレーション（ライブ経路は無し、既存の「ライブマイグレーション不要」
  方針のまま）。root diskはImageから移行先で作り直され、NetworkInterface（IP/MAC）
  とVolumeAttachment（Volumeデータ）はHypervisor非依存の参照モデルのまま無傷で
  引き継がれる。新phase`Migrating`（`Stopped → Migrating → Scheduled`と合流し、
  以降は既存のCreate/Start経路をそのまま再利用）、`scheduleMigration`
  （現在のHypervisorを自動選択から除外、または`target_hypervisor`明示指定時は
  同じ既存フィルタで検証）を追加。playgroundで実機確認済み: 実行中VMをStop→Migrate
  （自動選択）で別Hypervisorへ移し、Firecrackerゲストが新Hypervisor上で起動、
  NetworkInterfaceのIP/MACが移行前と完全に同一であること、旧Hypervisor側の
  jail/runディレクトリが後始末されることを確認（[VirtualMachine仕様]
  (specs/virtual-machine.md)「マイグレーション」参照）

## 2026-09-22

- `internal/compute-agent/imagestore.Store`にキャッシュエビクション（LRU＋参照カウント除外
  ＋サイズ閾値、`docs/architecture.md`「イメージのローカル管理」で既に決定していた方針）を
  実装。cache-hitのたびにmtimeを更新し（最終アクセス順の実現）、実行中VMがカーネルを
  直接参照し続ける間は`Pin`/`Unpin`で除外対象にし、`Sweep`が古い順に削除して
  `-image-cache-max-mb`（既定20GiB、0で無効化）を超えないようにする。compute-agentの
  `-image-cache-sweep-interval`（既定10分）タイマーで定期実行。compute-agent再起動時に
  `Reconcile`が拾い直す既存VMも`vmm.BootRecord.PinnedKeys`経由で正しく再pinされる
  （[Image仕様](specs/image.md)「ローカルキャッシュのエビクション」参照）

## 2026-09-20

- `VirtualMachineService.Resize`/`AttachVolume`/`DetachVolume`にライブ経路を追加。
  `Running`+`driver_hint=CLOUD_HYPERVISOR`のVMに対し、cloud-hypervisorの
  `--api-socket`経由でダウンタイム無しのvcpu/memoryリサイズ・Volume着脱ができる
  ようになった（コールド経路と同じRPCでphase/driver分岐、`Stopped`のVMは従来通り
  コールド動作）。Firecrackerは構造的にホットプラグ不可能なため対象外。
  新規`internal/compute-agent/chapi`（cloud-hypervisor api-socketクライアント）・
  `internal/compute/liveops.go`（`Reconciler`側の実装）を追加
  （[VirtualMachine仕様](specs/virtual-machine.md)「リサイズ」「Volume
  attach/detach」、[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)
  「`--api-socket`」参照）

## 2026-09-19

- `VolumeAttachment`の命名スキームを位置ベース（`volattach-<vm-id>-<index>`）から
  VolumeIDベース（`volattach-<vm-id>-<volume-id>`）へ変更。`AttachVolume`/`DetachVolume`
  実装に伴い、途中要素の削除で後続indexがずれ既存attachmentが孤児化するバグを回避するため
  （[Volume仕様](specs/volume.md)「compute側の統合」参照）
- VM Resize（コールド、Stopped限定）、Volume attach/detach（コールド）を実装

## 2026-09-15

- `chvmm`にUEFIブート（edk2の`CLOUDHV.fd`）を追加し、`QCOW2`（ブートローダー内蔵の
  自己完結ディスク）経由の起動をサポート。`fcvmm`は構造的に非対応のまま
  （[cloud-hypervisor起動仕様](specs/cloud-hypervisor-boot.md)「起動方式2: UEFIブート」参照）
- `kyuusha image build`を実装（`cmd/kyuusha/imagebuild.go`）。
  `docker build` → `docker export | tar -x` → `mkfs.ext4 -d` → `oras-go/v2`でOCIレジストリへ
  push → `ImageService.Create`という一気通貫パイプライン
  （[Image仕様](specs/image.md)参照）

## 2026-09-14

- イメージのローカル管理（Track 1）: fcvmm/chvmm共有の`internal/compute-agent/imagestore`
  を新設し、digest検証付きcontent-addressedキャッシュ・reflinkによるVM専用CoWコピーを実装。
  containerdの`content`/`snapshots`パッケージは依存が重すぎる・kyuushaの単一ディスクイメージ
  という要求と噛み合わないと判断し不採用、自前の小さなパッケージにした
  （[docs/architecture.md](architecture.md)「イメージのローカル管理」参照）
- イメージのOCIレジストリ対応（Track 2）: `oras-go/v2`採用、`ImageArtifact.url`に
  `oci://`スキームを追加

## 2026-09-13

- `network`/`block-storage`のreconcileループを、compute同様に単一インスタンスの
  別プロセス（`cmd/network-reconciler`/`cmd/block-storage-reconciler`）へ分離
- 認可ロールに`role=network-admin`（networkサービスにscopeしたadmin相当）・
  `role=viewer`（全テナント・全サービス横断read-only）を追加
- 払い出したリソース自身のメトリクス（VirtualMachine/NetworkInterface/Volume）を全種実装
- 孤児リソースGC（10分間隔の定期スイープ）をNetworkInterface/VolumeAttachmentへ適用
- バグ修正: `vm create -subnets=`がtap配線されないまま起動する不具合
  （NetworkInterfaceの非同期IP割り当てをcompute側が待たずbootへ進んでいたのが原因、
  `createNetworkInterfaces`に短時間ポーリングを追加して解消）

## 2026-09-12

- `VirtualMachineService.Stop`/`Start` RPCを実装
- pet/cattleの区別を廃止: `recovery_policy`/`persistent_root_disk`/`status.root_volume_ref`
  を削除（実質未使用だったフィールド。ハイパーバイザー喪失時の自動リカバリはKaaS層/
  オペレータに委ねる判断、[docs/architecture.md](architecture.md)「ハイパーバイザー死活監視と
  リカバリ、およびpet/cattleの区別の廃止」参照）
- VMMドライバをQEMU直接execからcloud-hypervisor直接execへ置き換え
- バグ修正: `Delete`後もjail/runディレクトリの実体が永久にリークし続けていた不具合
  （`handleDelete`が呼ぶメソッドを`Stop`から`Destroy`へ変更）

## 2026-09-11

- バッキングストアをオンメモリのmapからetcdへ移行。`internal/resource.Store`を
  etcd-backedに書き換え、5サービス全てが`-etcd-endpoints`経由で接続するよう変更
  （[docs/architecture.md](architecture.md)「コントロールプレーンサービス自体の可用性」参照）
- ハイパーバイザー向けの本格PKI（証明書動的発行）を不採用と確定。代わりにbootstrapトークンを
  軽量拡張し個体識別・失効を実現（将来の`Register`拒否のみ、既存セッションの強制切断は不可）
- 認可ロールに`tenant_role=viewer`（テナント内read-only）・`role=storage-admin`
  （block-storageサービスにscopeしたadmin相当）を追加
- block-storageに`StorageConnection`リソースを新設し、Volume作成時の到達性/実在性検証を
  非同期Pending→Readyパターンで実現
  （[docs/architecture.md](architecture.md)「『参照するだけ』の弱点をStorageConnection
  リソースで埋める」参照）

## 2026-09-10

- block-storageの責務境界を「プロビジョニング＋export」から「参照＋接続」へ縮小。
  専任サービス`storage-agent`（`StorageBackend`ドライバ抽象化でプロビジョニング・export・
  compute-agent側のiSCSI/NVMe-oFイニシエータ接続まで担っていた）を削除。
  「利用組織ごとに選びたいストレージバックエンドが全く違う」という現実を軽視した設計だった
  ため（[docs/architecture.md](architecture.md)「block-storageのバックエンド抽象化」参照）

## 2026-09（初版）

- 専用ストレージノード + iSCSI/NVMe-oF（ZFSバックエンド）方式でblock-storageを実装
  （後日2026-09-10に責務ごと削除、上記参照）
