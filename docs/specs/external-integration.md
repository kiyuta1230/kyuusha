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
| ゲート系・作成側（作成前に外部バリデーションを通したい） | 独自ポリシーチェックをCreate前に挟む | **未実装**（ValidatingAdmissionWebhook相当、設計のみ） |

## 通知系: Watch

全リソースの`Watch(tenant_id, since_resource_version)`は、`resource_version`から
再開できるリプレイ+ライブストリーム。OpenStackのRabbitMQ notificationと違い、
外部コントローラーが一時的に落ちていても取りこぼさず、`since_resource_version`を
自分のDBに保存しておいて再接続時にそこから再開すればよい。新しい実装は不要——
既存のWatch RPCをそのまま使う。

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

### 現状の対応範囲: VirtualMachineのみ

Finalizer機構自体は`internal/resource.Store`（全リソース共通の汎用実装）にあり
どの型でも使えるが、実際に意味のある形で使えるのは**VirtualMachineだけ**。

- `compute.Service.Delete`はFinalizerが残っている時、`store.Delete`を呼ぶ前に
  `status.phase`を`Deleting`へ遷移させる。`tenant_usage`（Quota使用量）は
  Delete呼び出し時点で減算する——Finalizerが解放されて実際にオブジェクトが
  消えるタイミングではない（同一テナント内で一時的にQuotaの余裕が実態より
  多く見える、という無害な近似。詳細はコード中のコメント参照）
- Subnet/NetworkInterface/Volumeの各`Delete`は、VLAN/IPプールの解放や
  `tenant_usage`減算を無条件かつ即座に行っており、これらにFinalizerを付けると
  プール割当だけ先に解放される整合性の穴がある（`docs/architecture.md`
  「Finalizer」節「既知の穴」参照）。今のところこれらの型にFinalizerを付ける
  経路（CLI等）が無いため実害はないが、対応は個別に必要——**現状これらの型に
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

## ゲート系(作成側): 未実装

Create前の同期的な外部バリデーション（Kubernetesの`ValidatingAdmissionWebhook`
相当）は設計のみで実装していない。Finalizerより複雑（同期的な外部呼び出しに
伴う可用性のカップリング、timeout/failure policy設計、「誰がwebhookを登録
できるか」というセキュリティ）なため、後回しにしている。今のCreate時
バリデーション（Image/NetworkInterface/Quota等）はすべてkyuusha内部にハード
コードされており、外部から差し込む口は無い。
