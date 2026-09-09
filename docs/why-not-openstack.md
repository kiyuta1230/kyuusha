# なぜOpenStackではなくkyuushaか

このページは、`docs/architecture.md`の各所に散らばっている「OpenStackのここが辛い→
kyuushaではこう解決した」という判断を1箇所にまとめた比較表。各判断の詳しい経緯・
議論・トレードオフはリンク先の`docs/architecture.md`本編を参照——ここでは結論だけを並べる。

## 対象読者・想定スケール

「OpenStackを導入するには大きすぎる（専任運用チームを抱えられない）が、VMwareは
一定規模からライセンスコストが厳しくなる」という間に落ちる企業の、単一組織の
社内プライベートクラウド（赤の他人同士が同居する公開マルチテナントクラウドではない）。

想定スケール（1デプロイ＝1リージョン相当）: ハイパーバイザー〜500台、VM〜1〜2万台、
テナント(KaaSクラスタ)〜500。この前提の上に、以下の全ての機能縮小判断が乗っている
（[docs/architecture.md](architecture.md)「想定するユーザー像とスケール」参照）。

## 比較表

| 領域 | OpenStackの辛さ | kyuushaの答え |
|---|---|---|
| VMのライフサイクル前提 | Novaは「VMが最終利用者に長期間ペットとして使われる」前提でライブマイグレーション等フル機能を持つ | KaaSクラスタへハイパーバイザーを供給するだけに機能を絞る。ハイパーバイザー障害の復旧はKaaS層(Pod再スケジュール)が担い、ライブマイグレーションは不要（[architecture.md](architecture.md)「コンセプト」参照） |
| ディスク永続化 | Cinderがスナップショット/レプリケーション等フル機能を持つ | 最小限。ルートディスクはイメージからのephemeral/copy-on-write、永続化が要る場合のみVolumeをattach（[architecture.md](architecture.md)「スコープ縮小の判断」参照） |
| テナントネットワーク | Neutronがoverlay per-tenant/router/floating IP/per-tenant security policyまでフル機能を持つ | 最小限。テナント＝KaaSクラスタ単位のL2/L3分離のみ。クラスタ内Pod間の分離はCNI/NetworkPolicy層(KaaS側)の責務（同上） |
| APIエンドポイント | Keystoneのサービスカタログ方式——各コンポーネントが個別のpublic URLを持ち、クライアントがカタログを見て使い分ける | `api-gateway`という単一の公開エンドポイントに集約。認証・認可の実施点も1箇所（backendは無認証）。K8s API server/BFFパターンと同じ思想（[認証・認可仕様](specs/authn-authz.md)参照） |
| API設計 | REST、命令的CRUD+ポーリング、プロジェクトごとに規約バラバラ | 宣言的API（spec/status分離、Watch、resource_versionによる楽観的並行性制御）。ただしKubernetes CRD/Aggregated API Serverそのものにはしない（[architecture.md](architecture.md)「スコープ縮小の判断」参照） |
| リソースの固定カタログ | Flavorという料理的比喩の間接層。実装都合の語彙で意味が読み取れない | `VirtualMachineSpec.vcpu`/`memory_mb`を直接指定。固定カタログ自体をQuotaと役割重複と判断し廃止（[architecture.md](architecture.md)「設計原則: 命名はOpenStackを踏襲しない」参照） |
| リソース命名 | Port（仮想スイッチの差し込み口という実装比喩）等、実装都合由来の用語が多い | 実態を直接表す語を選ぶ（`Port`→`NetworkInterface`等）。ただし`Volume`/`Subnet`のような業界共通語はそのまま使う（同上） |
| Web UI | Horizonは長年APIの新機能に追従できず、多くの運用者がCLI/APIを直接叩いている実態がある | 自前Web UIは作らない。CLI（`kubectl`/`terraform`的）+ 既存のPrometheusメトリクスをGrafanaで可視化（[architecture.md](architecture.md)「UI: 自前のWeb UIは作らない。CLIとGrafanaに任せる」参照） |
| テレメトリ基盤 | Ceilometerは自前基盤を一から作り、MongoDB→Gnocchi→Aodh→Pankoと分裂・作り直しを繰り返した。定期ポーリングが監視対象に負荷をかけ、RabbitMQ通知が制御プレーンの輻輳に直結した | 自前基盤は作らずPrometheus(pull型`/metrics`)+OpenTelemetryという業界標準に乗る。トレーシングは後付けでなく最初から設計に組み込む（[architecture.md](architecture.md)「Observability: OpenStack(Ceilometer)を反面教師にする」参照） |
| 外部システム連携（通知系） | RabbitMQ notificationは取りこぼしが起き得て、resumeできない | 既存のWatch（`resource_version`からの再開）で対応。取りこぼしても再開できるため専用の仕組みは不要（[architecture.md](architecture.md)「Finalizer: 外部システムによる削除ブロック」参照） |
| 外部システム連携（ゲート系） | Nova/Neutronに標準の拡張点がなく、パッチを当てるかポーリングでDBを覗く無理なやり方しかない | 削除側はKubernetesと同じ**Finalizer**パターンをそのまま借用（同上） |

## 非ゴール（正直な線引き）

上記はいずれも「想定スケール・想定ユーザー」という前提の上での判断であり、OpenStackを
全面的に置き換えることを目指したものではない。以下は意図的にスコープ外:

- 赤の他人同士が同居する公開マルチテナントクラウド（テナント分離の脅威モデルが違う）
- 数千ハイパーバイザー・10万VM超のような1桁上のスケール（スケジューラのキャッシュ/インデックス戦略、DBシャーディング等、別の前提の見直しが要る）
- OpenStackが持つドライバ/プラグインの広いエコシステム（ストレージバックエンド、ネットワークバックエンド等の選択肢の広さ）

詳しい経緯・議論・トレードオフは[docs/architecture.md](architecture.md)を、現状の
機能単位の仕様は[docs/specs/](specs/README.md)を参照。
