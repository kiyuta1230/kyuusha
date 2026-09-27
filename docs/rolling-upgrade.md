# コントロールプレーンのローリングアップグレード手順

## 対象読者

kyuushaのコントロールプレーン（api-gateway/compute/identity/image/network/
block-storageとそれぞれの`*-reconciler`、compute-agent）を実際に運用し、
新しいバージョンへ入れ替える作業を行う運用者向け。

## 前提: 3種類の層で可用性モデルが違う

`docs/architecture.md`「コントロールプレーンサービス自体の可用性」で確定した
設計をそのまま踏まえる。**この手順書はその設計の上に立つ「どの順で、どう
入れ替えるか」のオペレーション面だけを扱う**——なぜAPI面がステートレス複製で、
reconcile面がプロセス分離+単一インスタンスなのか、といった設計判断の理由は
architecture.md側を参照。

| 層 | 具体的なプロセス | 複製 | アップグレード方式 |
|---|---|---|---|
| API面 | `api-gateway`/`compute`/`identity`/`image`/`network`/`block-storage` | N台+ロードバランサ（本番想定。playgroundは各1台のみ） | 真のローリング（1台ずつ、他は稼働継続） |
| reconcile面 | `compute-reconciler`/`network-reconciler`/`block-storage-reconciler` | 常に1台のみ（設計上の制約） | recreate（旧を完全に止めてから新を起動、重複起動は絶対に避ける） |
| ハイパーバイザー | `compute-agent`（各ホストに1つ） | ホスト数分 | ホストごとに順次、**コンテナ/Pod自体を作り直さない**在地バイナリ入れ替え |

## 1. API面（compute/network/block-storage/identity/image/api-gateway）

各gRPCハンドラは状態を一切持たない（`docs/architecture.md`「API面: 素直に
ステートレス複製」）ため、標準的なローリングアップグレードがそのまま使える:

1. ロードバランサ配下の1台を切り離す（ヘルスチェック失敗にするか、明示的に
   ルーティングから外す）
2. その1台だけ新バージョンへ入れ替えて起動
3. 起動確認（`/metrics`または実際に1回`Get`系RPCを叩く）できたらロード
   バランサへ戻す
4. 残りの台数分、1台ずつ繰り返す

**同時に複数台を落とさない**——ロードバランサ配下に最低1台は常に残す。
playgroundのように1台構成の環境では、この手順は「真のローリング」ではなく
「recreate（短い停止を伴う）」になる——それ自体は許容し、本番では複数台構成に
すべき、という運用要件として明記しておく。

**api-gatewayだけの注意**: 新しいRPCをバックエンド側（例:
`compute`/`network`等）に追加した場合、api-gateway自身にもプロキシ実装の
追加が要る（過去に一度、新RPCの実装漏れでapi-gateway経由だけ`Unimplemented`に
なったことがある——`docs/release-notes.md`参照）。バックエンドを先に上げて
api-gatewayを後にする分には問題ないが、**逆（api-gatewayだけ先に新RPC対応
コードを持ち込み、バックエンドがまだ対応していない）は、そのRPCを呼ぶまでは
実害が出ない**ため、どちらの順でも壊れはしない。ただし新RPCを実際に使い始める
タイミングは「バックエンドとapi-gateway両方が新バージョンになった後」に
すること。

## 2. reconcile面（`*-reconciler`）

**常に1インスタンスのみ**という設計上の制約（`docs/architecture.md`
「Reconcile面」）を、アップグレード中も一瞬たりとも破ってはならない——
2つのreconcilerプロセスが同時に同じVirtualMachine/Subnet/VolumeAttachmentを
処理しようとすること自体は`resource_version`の楽観的並行性制御が最悪の破損を
防ぐが、「無駄な競合が常態化する」（同docs)という設計上望ましくない状態を
アップグレード中とはいえ意図的に作る理由が無い。そのため**ローリングではなく
recreate**で行う:

1. 旧バージョンのプロセスを止める
2. **プロセスが完全に終了したことを確認する**（コンテナなら`docker ps`等で
   消えたことを見る、systemdなら`systemctl is-active`がinactiveになるのを待つ）
3. 新バージョンのプロセスを起動する

ステップ2を飛ばして「新を起動してから旧を止める」順にしない——その一瞬でも
2インスタンス目が存在する時間帯を作らないのが目的。

止まっている間、reconcileは進まない（新規VMのスケジューリングやHypervisor
死活監視スイープが一時停止する）が、**API面は影響を受けず稼働し続ける**
（Create/Get/List/Watch/Delete等は別プロセスなので普通に応答する)。止まっていた
間に溜まった`Pending`状態のリソースは、新プロセス起動後の初回`Watch`
バックログ再生とperiodicなretry sweepの両方で自然に拾われる——実機確認済み
（下記「playground実地確認」参照）。

## 3. compute-agent（各ハイパーバイザー）

**最も注意が要る層**。compute-agentが管理する実VMプロセス（Firecracker/jailer、
cloud-hypervisor）はcompute-agent自身の子プロセスではなく独立している
（`internal/compute-agent/vmm.BootRecord`のパッケージdocコメント: 「jailer/
VMMバイナリはexec'd directly、共有PID名前空間は無い」)——**compute-agent
プロセス自体を再起動しても、その再起動が起きたホスト/ネットワーク名前空間の
外側からVMプロセスを巻き込まない限り、VMは生き続け、次の起動時に
`Reconcile()`が自動的にadopt（再認識）する**（`internal/compute-agent/fcvmm`/
`chvmm`の`Reconcile`、実際にプロセス再起動を跨いでadoptされることを新設の
`TestManagerReconcileAdoptsRunningProcessAcrossRestart`で確認済み——下記参照）。

**この前提が崩れる具体的なケース: コンテナ/Pod自体の作り直し**。Dockerの
`docker compose restart compute-agent-N`やコンテナの再作成は、compute-agent
プロセスだけでなく**そのコンテナのネットワーク名前空間ごと破棄し、コンテナ内の
全プロセス（Firecracker/jailerの子プロセスも含む）を道連れに終了させる**——
これはDockerコンテナ特有の挙動で、ベアメタル+systemd環境で「compute-agent
プロセスだけ」を再起動する場合とは全く違う（`docs/specs/snap.md`で既に
確認済み）。**現状のplayground/`docker/Dockerfile`のcompute-agentステージは
compute-agentバイナリ自身がコンテナのPID 1**（supervisorを挟んでいない）ため、
**このコンテナ構成のままではin-placeなバイナリ入れ替えができない**——
バイナリを入れ替えるには結局コンテナ自体を作り直すしかなく、それは即ち
そのホスト上の全VMを巻き込んで落とすことを意味する。

現時点での現実的な選択肢:

- **ベアメタル/VM + systemdでcompute-agentを直接デプロイする**（コンテナ化
  しない）: `systemctl restart compute-agent`はプロセスだけを再起動し、
  ネットワーク名前空間はホスト自体のものなので破棄されない。VMは生き続け、
  `Reconcile()`がadoptする。**現時点で唯一、実際に検証できている安全な経路**
- **コンテナ化する場合**: compute-agentをコンテナのPID 1にしたまま安全な
  in-placeアップグレードはできない。tini/dumb-init等の軽量supervisorを挟んで
  compute-agentをその子プロセスにし、supervisor自身は生かしたまま
  compute-agentだけをkillして再起動する、という構成変更が要る——ただし
  これはシグナル転送・ヘルスチェック・ログの扱いに影響する構成変更であり、
  今回は行わなかった（`docs/open-questions.md`「compute-agentコンテナへの
  supervisor導入」参照、意図的な先送り）

ホストごとの実際の手順（ベアメタル/systemd前提）:

1. アップグレード対象ホストを新規スケジュール対象から除外したい場合、
   Hypervisorリソースの`schedulable`相当のフラグを操作する（既存の
   スケジューラのフィルタ条件、`docs/specs/vm-scheduling.md`参照）——
   必須ではないが、アップグレード中に新規VMがそのホストへ割り当たるのを
   避けたいなら行う
2. 新しいcompute-agentバイナリを配置する
3. `systemctl restart compute-agent`（またはそれに相当する操作）
4. compute-agentのログで「adopted a VM still running from a previous
   compute-agent process」が既存VMの数だけ出ることを確認する
5. `kyuusha hypervisor get`等で該当Hypervisorが`Ready`に戻ることを確認する

## wireプロトコルの互換性ポリシー

アップグレード中は新旧バージョンのプロセスが同時に生きている時間帯が必ず
発生する（reconcile面の一瞬を除く）。これを安全にするための、今後の変更に
対する規約:

- protoのフィールドは**追加のみ**。既存フィールドの番号を再利用したり、
  削除したりしない（protobufの標準的な後方/前方互換性保証にそのまま乗る:
  新フィールドは旧コードから見れば単に無視される、旧メッセージを新コードが
  読んでも新フィールドはゼロ値になるだけ）
- 新しいRPCメソッドの追加はいつでも安全（呼ばれるまで実害が無い）。ただし
  api-gateway側のプロキシ実装も**同じPRで**追加すること（上記「API面」の
  注意参照）
- 既存RPCの引数・返り値の意味を変える（同じフィールド名のまま意味を変える等）
  破壊的変更は、ローリングアップグレード中に新旧混在で叩かれても安全な形に
  必ずできるとは限らない——このような変更が必要になった場合は、まずこの節に
  個別の移行手順を書き足すこと

## playground実地確認

以下を実機で確認済み:

- **reconcile面のrecreate**: `vm create`で1台VMを作成し`-wait`を付けず
  `Pending`のまま放置、直後に`docker compose stop compute-reconciler`
  （旧プロセスの完全停止を確認）→`docker compose up -d compute-reconciler`
  （新プロセス起動）と実行——停止していた間もAPI面（`compute`）は
  `vm get`に応答し続け、reconciler再起動後にそのVMが自然に
  `Scheduled`→`Provisioning`→`Running`まで進んだ（取りこぼし無し）
- **API面のrecreate（1台構成での短時間停止）**: バックグラウンドで
  `vm get`を継続ポーリングしながら`docker compose stop/up compute`を
  実行——停止中は接続エラーが（想定通り）返り、起動後は自然に応答が
  再開し、状態の欠落は無かった
- **compute-agentのadopt機構自体**: `internal/compute-agent/fcvmm`/
  `chvmm`それぞれに`TestManagerReconcileAdoptsRunningProcessAcrossRestart`
  を新設し、実プロセスの生存を跨いだadopt→teardownを確認（Firecracker/
  cloud-hypervisor本体の起動は不要な、adopt機構そのものの検証）

**compute-agent自体のin-placeアップグレード**（コンテナ作り直し無しでの
バイナリ入れ替え）は、上記「compute-agent（各ハイパーバイザー）」節の通り
現状のコンテナ構成では検証できない（構成変更が必要なため）——ベアメタル/
systemd環境での実地確認は今後の課題として残る。
