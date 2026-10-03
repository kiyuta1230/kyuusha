# 外部システム連携仕様

## 概要

CMDB登録、ネットワーク台帳登録、独自バリデーション、削除前チェックのような、kyuusha
の外側にある既存システムとの連携ニーズに対して、kyuushaがどの仕組みを提供する（/しない）
かをまとめる。背景は`docs/architecture.md`「Finalizer: 外部システムによる削除ブロック」
の「きっかけ」節を参照——OpenStackでの実運用経験から、この手の連携が「無視できない」
と判断して設計・実装した。

外部システム連携のニーズは大きく2種類に分かれ、kyuushaでは別々の仕組みで対応する。

| ニーズの種類 | 例 | kyuushaの対応 |
|---|---|---|
| 通知系（何が起きたかを確実に拾いたい） | VM起動完了でCMDB登録、IP払い出しでネットワーク台帳登録 | **Watch**（実装済み、全リソース共通） |
| ゲート系・削除側（外部の確認が取れるまで実削除させたくない） | VM削除時、そのインターフェースのIPが外部ACLにまだ残っていたら拒否 | **Finalizer**（実装済み、現状VirtualMachineのみ） |
| ゲート系・作成側（作成前に外部バリデーションを通したい） | 独自ポリシーチェックをCreate前に挟む | **Admission Webhook**（`internal/admissionwebhook`、現状VirtualMachineのみ） |

## ラベルとアノテーション

Subnet・NetworkInterface・VirtualMachineは`meta.labels`/`meta.annotations`（任意の
key/value）を持てる。kyuushaの上に載る外部ソフトウェアが「このSubnetはどのVPCに属するか」
のような自分の情報を記録するためのもので、kyuusha自身は中身を一切解釈しない（`vpc_id`の
ような専用フィールドを足してkyuushaにVPC等の概念を持ち込まないための仕組み）。

- **設定**: Createリクエストの`labels`/`annotations`で作成時に付けるか、Updateで
  `meta`ごと丸ごと置き換える（finalizersと同じread-modify-write）。名前による冪等な
  再Createでは、specと同様に無視される。CLIは`subnet`/`netif`/`vm create`の
  `-labels`/`-annotations`（`k=v,k2=v2`）
- **キー**: `[prefix/]name`。prefixはDNSサブドメイン（小文字、253文字以下）、nameは
  1〜63文字の`[A-Za-z0-9._-]`で両端は英数字（Kubernetesのラベルと同じ規則）
- **値**: ラベルは63文字以下の同じ文字種（空も可）、最大64個。アノテーションの値は
  自由だが、キーと値の合計で64KiBまで
- **テナントが書き換えられる**: ラベルはオブジェクトの他の部分と同じくテナント自身が
  書ける。テナントに偽造されては困る情報（「このSubnetはVPC Xに属する」が認可の根拠に
  なる等）は、ラベルだけを信用せず、Admission Webhookで変更を検証するか、外部ソフト
  ウェア側で正となる台帳を持つこと
- **VNAP/SNAPへの伝搬**: Subnetのラベルは`subnet_labels`としてVNAP/SNAPのattach
  payloadに載る（[VNAP仕様](vnap.md)・[SNAP仕様](snap.md)）
- 上記3種類以外のリソースでは、protoの`ObjectMeta`にフィールドはあるが常に空
  （保存されない）

## 通知系: Watch

全リソースの`Watch(tenant_id, since_resource_version)`は、`resource_version`から
再開できるリプレイ+ライブストリーム。OpenStackのRabbitMQ notificationと違い、
外部コントローラーが一時的に落ちていても取りこぼさず、`since_resource_version`を
自分のDBに保存しておいて再接続時にそこから再開すればよい。新しい実装は不要——
既存のWatch RPCをそのまま使う。

### 全テナント横断のList/Watch

テナントにスコープを持つリソース（VirtualMachine/Subnet/NetworkInterface等）の
`List`/`Watch`は、`tenant_id`を空にすると**全テナント横断**で返す——1プロセスで
全テナントを見る外部コントローラー向けの正式な契約で、`resource_version`からの再開も
テナント単位のWatchと同じく効く（`since_resource_version`は全テナント共通のetcd
revisionなので、横断Watchでもそのまま使える）。

空の`tenant_id`は認可上「テナントを指定しないリクエスト」として扱われるので、
通るのはテナント横断のroleを持つトークンだけ（[認証・認可仕様](authn-authz.md)の
「リクエストメッセージが`tenant_id`フィールドを持たない場合」と同じ規則）: `admin`
（全サービス）、`network-admin`（networkサービスのみ）、`storage-admin`
（block-storageのみ）、`viewer`（全サービスのread系のみ）。CLIでは`vm`/`subnet`/
`netif`の`list`/`watch`に`-all-tenants`を付ける。

## ゲート系(削除側): Finalizer

設計の背景・仕組み全体は`docs/architecture.md`「Finalizer」節を参照。ここでは
実際に触る側（外部コントローラー実装者）の視点でまとめる。

### 使い方

1. 外部コントローラーは自分の識別名（例: `"acme.corp/network-acl-cleanup"`）を
   決めておく。命名規則の強制はない（Kubernetesの`<domain>/<name>`風を推奨する
   だけ）
2. 対象VMをWatch（またはGet）し、`meta.finalizers`に自分の識別名が無ければ
   `Update`で追加する。エントリの`added_by`はクライアントが指定しても無視され、
   呼び出し元の実JWT `sub`がサーバー側で刻まれる（「認可」節参照）
3. 誰かがそのVMを`Delete`すると、`meta.deleted_at`がセットされる（実削除は
   されない）。外部コントローラーはWatchでこれを検知する
4. 後処理（外部ACLの確認・解放など）を行い、完了したら`meta.finalizers`から
   自分の識別名だけを取り除いて`Update`を呼ぶ。追加した時と同じ`sub`（または
   admin role）でないとこの削除は拒否される
5. `finalizers`が空になった時点で、そのUpdate呼び出しの中で実際に削除され、
   `Deleted`イベントが出る

新しいRPCは無い——既存の`Get`/`Update`/`Watch`だけで完結する。

### 大量Watch対策: finalizer_name

外部コントローラーが自分のFinalizerの状態を知りたいだけなのに、テナント内の
全VMをWatchして自分のFinalizer名が含まれるかを毎回自分でフィルタするのは、
500テナント・2万VM規模では無駄が大きい。`WatchVirtualMachinesRequest`に
`finalizer_name`（省略可）を渡すと、サーバー側で「現在`meta.finalizers`に
その名前を含むVMだけ」に絞り込んだリプレイ+ライブストリームになる
（`kyuusha vm watch -tenant=... -finalizer-name="acme.corp/network-acl-cleanup"`）。

**評価タイミングに注意**: フィルタは「そのイベントのVMスナップショットが今
その名前を含むか」で判定される。自分がFinalizerを追加してから、`deleted_at`
セット→自分がFinalizerを取り除くまでの一連のイベントはすべて見えるが、
自分がFinalizerを取り除いた（＝その名前が消えた）Update自体の結果や、それ以降の
イベントはフィルタの対象から外れる——ただしこれは自分の操作の結果であり、
その`Update`呼び出し自体の戻り値で分かるので実害はない。一方、admin role
または`added_by`が空文字列の後方互換経路で**自分の知らないところ**でFinalizerを
外された場合は、このフィルタでは検知できない（単に何も届かなくなるだけ）。

### CLI

```sh
kyuusha vm add-finalizer -tenant=... -id=... -finalizer="acme.corp/network-acl-cleanup"
kyuusha vm remove-finalizer -tenant=... -id=... -finalizer="acme.corp/network-acl-cleanup"
```

どちらもGet→ローカルで`finalizers`を変更→Updateという素朴な実装。ただし
サーバー側（`compute.Service.Update`）が`added_by`のスタンプ・所有権チェックを
行うため、単純なフィールド上書きではない（「認可」節参照）。`resource_version`の
競合時にリトライはしない（一発実行のCLIツールであり、コントローラーループでは
ないため、競合はそのまま呼び出し元に返す）。

`kyuusha vm get`/`list`/`watch`の出力には`finalizers=...`と`deleted_at=...`が
表示される。

### 現状の対応範囲: VirtualMachine・Subnet・NetworkInterface

Finalizer機構自体は`internal/resource.Store`（全リソース共通の汎用実装）にあり
どの型でも使えるが、実際に意味のある形で使えるのは**VirtualMachine・Subnet・
NetworkInterface**（Updateでの追加・削除、所有者チェック、削除待ちの間の資源保持が
揃っているもの）。CLIは`vm`と`subnet`の`add-finalizer`/`remove-finalizer`。

- `compute.Service.Delete`はFinalizerが残っている時、`store.Delete`を呼ぶ前に
  `status.phase`を`Deleting`へ遷移させる。`tenant_usage`（Quota使用量）は
  Delete呼び出し時点で減算する——Finalizerが解放されて実際にオブジェクトが
  消えるタイミングではない（同一テナント内で一時的にQuotaの余裕が実態より
  多く見える、という無害な近似。詳細はコード中のコメント参照）
- Subnet/NetworkInterfaceは、Finalizerで削除が止まっている間もVLAN ID/IPを保持し
  続け、実際に消えた時点で初めてプールへ返却する（[network仕様](network.md)
  「IPAM」参照）。`tenant_usage`の扱いはVirtualMachineと同じ近似
- Volumeの`Delete`は資源の解放と`tenant_usage`減算を無条件かつ即座に行っており、
  Finalizerを付けると割当だけ先に解放される整合性の穴がある（`docs/architecture.md`
  「Finalizer」節「既知の穴」参照）——**現状Volumeには
  Finalizerを使わないこと**

### 認可: 削除できるのは追加した本人かadminだけ

Finalizerエントリの削除は「追加したのと同じ呼び出し元（JWT `sub`）」または
admin roleに限定されている（`compute.Service.Update`の`checkFinalizerMutation`）。
追加時に刻まれる`added_by`はクライアントが指定しても無視され、api-gatewayが
検証したJWTの`sub`がサーバー側で刻まれる——つまりテナント自身の別トークンで
勝手に他者のFinalizerを消すことはできない。この`sub`はapi-gatewayでしか
手に入らないため、`internal/authn/propagate.go`のgRPCクライアントインター
セプターがapi-gateway→backend間の信頼済みメタデータとして転送する（詳細は
`docs/architecture.md`「Finalizerの所有権」節）。

例外: `added_by`が空文字列（このplumbing導入前に付いたエントリ、または
api-gatewayを経由しない内部呼び出しから付いたエントリ）の場合は所有者不在
として誰でも削除できる——後方互換のためであり、新規に追加するエントリが
この状態になることは通常ない（api-gateway経由なら必ず`sub`が刻まれる）。

## ゲート系(作成側): Admission Webhook

書き込み前の同期的な外部バリデーション（Kubernetesの`ValidatingAdmissionWebhook`
相当）。`internal/admissionwebhook`が実装し、各サービスが内部バリデーション
（Quota等）を全て通した後・実際に永続化する前の最後のゲートとして呼ぶ
（Quotaと同じ「doomedなリソースを作ってからErrorにしない」設計に従う）。

### 契約

- **呼び出し**: 設定された全URLへ並行にHTTP POST（JSON）。**全てが`allowed: true`
  を返して初めて許可**——Kubernetes自身の「全てのvalidating webhookが合意して
  初めて許可」と同じ意味論（「どれか1つの承認で十分」ではない）
  ```json
  {"operation":"UPDATE","resource":"Subnet","tenant_id":"...","name":"...","id":"subnet-...",
   "labels":{...},"annotations":{...},"spec":{...},
   "old_object":{"id":"...","name":"...","tenant_id":"...","labels":{...},"annotations":{...},
                 "spec":{...},"status":{...}}}
  ```
  `operation`は`CREATE`/`UPDATE`/`DELETE`。`labels`/`annotations`/`spec`は書き込み後の
  状態（`DELETE`では`spec`無し）、`old_object`は現在保存されている状態
  （KubernetesのAdmissionReviewの`oldObject`相当。`CREATE`では無し、`id`も
  まだ無い）。`spec`/`status`はsnake_caseのJSON
  レスポンス:
  ```json
  {"allowed":false,"reason":"..."}
  ```
- **gRPCではなくHTTP+JSON**: 外部バリデータはkyuushaが実装言語・スタックを
  制御できない第三者ツールが典型なので、生成されたgRPCクライアントを要求せず
  Kubernetes自身のAdmissionReviewと同じ素朴なHTTP規約にした
- **Validatingのみ、Mutatingではない**: `docs/specs/external-integration.md`が
  当初から「ValidatingAdmissionWebhook相当」と明記していた通り、返せるのは
  許可/拒否のみ。specを書き換えるMutating側の意味論（merge-patch等）は実装しない
- **失敗時の挙動は設定可能**: webhookが疎通不能/タイムアウトした場合、既定
  （`-admission-webhook-fail-open=false`）はfail-closed（拒否）。true にすると
  fail-open（暗黙に許可）——ただし**明示的な拒否は他のwebhookが疎通不能でも
  常に優先される**（fail-openは「答えが得られない」場合の話であり、「はっきり
  ノーと言われた」場合を上書きしない）
- **タイムアウト**: `-admission-webhook-timeout`（既定3秒）。webhookごとに独立
- **セキュリティ**: webhook URLの一覧は`compute`/`network`サービス起動時のオペレータ
  設定（`-admission-webhook-urls`、カンマ区切り。サービスごとに別々に設定）のみ——APIからテナントが
  登録する経路は無い。「誰がwebhookを登録できるか」という当初の懸念を、
  `internal/compute-agent/netsetup`のVNAPプラグイン（`-network-attach-bin`）
  と同じ「オペレータ設定のみ、テナント非公開」という考え方で回避している
- **エラーマッピング**: 明示的な拒否は`PermissionDenied`（理由を含む）、
  webhook疎通不能によるfail-closedは`Unavailable`

### 現状の対応範囲

| サービス | リソース | 操作 |
|---|---|---|
| `compute` | VirtualMachine | `Create` |
| `network` | Subnet | `Create`/`Update`/`Delete` |
| `network` | NetworkInterface | `Create`/`Update`/`UpdateFirewallRules`（いずれも`UPDATE`として届く） |

NetworkInterfaceの`Delete`はゲートしない——VM削除時のcompute→networkの後始末や
networkのorphan GCも同じ経路を通るため、webhookの拒否がVM削除を止めてしまう。
削除を止めたい用途にはFinalizerを使う。他のリソース種別・操作への展開は、
実需が出た時点で追加する。
