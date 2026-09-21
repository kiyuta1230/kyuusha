# リリースノート

このプロジェクトの「いつ・何が変わったか」の記録。**設計判断の「なぜ」は
[docs/architecture.md](architecture.md)、現状の仕様は[docs/specs/](specs/README.md)を
参照**——ここには日付付きの事実のみを置き、設計トレードオフの深掘りはarchitecture.mdへ
リンクする形にする。

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
