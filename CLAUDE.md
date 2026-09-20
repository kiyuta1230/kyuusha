# CLAUDE.md

kyuushaで作業するAIエージェント向けのルール。

## ドキュメントの棲み分け

4種類のドキュメントは役割を厳密に分ける。どれに何を書くか迷ったら以下で判断する。

| ドキュメント | 役割 | 書かないもの |
|---|---|---|
| [docs/specs/](docs/specs/README.md) | 完成した機能単位の**現状の仕様**のみ | 過去の経緯・理由・日付 |
| [docs/architecture.md](docs/architecture.md) | 設計判断の**「なぜ」**の記録（経緯・議論・トレードオフ）。OpenStack同様のマイクロサービス分割によるIaaSの全体像を、他プロジェクト調査目的でまず固めるのが目的。**設計レベルの**未決事項もここの「未決事項」節に置く | 日付（「いつ実装したか」等）。実装フェーズの細かい迷い事項（下記open-questions.md参照） |
| [docs/open-questions.md](docs/open-questions.md) | **実装を進める中で出てきた**、まだ判断を保留している細かい話のメモ。随時追加・解決済みなら随時消し込む作業ノート | 設計レベルの論点（architecture.mdへ格上げする） |
| [docs/release-notes.md](docs/release-notes.md) | **「いつ何が変わったか」**の日付付き変更履歴 | 設計の理由の深掘り（architecture.mdへリンクするだけ） |

- `docs/architecture.md`に「### 追記（2026-09-15）: ...」「### 訂正: ...」のような
  変更履歴的な章立てを作らない。訂正が必要な記述を見つけたら、その場で現在の設計として
  直接書き直す（「以前はXだったが誤りでYにした」ではなく、最初からYを説明する形にする）。
  日付が伴う事実（実装完了日、バグ発見日等）はrelease-notes.mdへ書く
- `docs/open-questions.md`はarchitecture.mdと違って**作業ノート**という性質上、
  見出しに解決日を書く（例:「〜すべきか（解決済み・実装済み、2026-09-19）」）のは許容する
  ——ここは折に触れてではなく実装の都度更新し、解決済み項目は次の整理で削って構わない。
  設計判断として恒久的に記録すべき重さの論点だと分かったら、architecture.mdの「未決事項」
  （こちらは日付を持たない）へ格上げして、open-questions.md側は消す
- 実装を変更したらdocs/specs/の対応する仕様書を都度追従して更新する（architecture.mdは
  都度ではなく折に触れて更新すれば良い、というのは既存の合意通り）

## 変更を入れるときに必須のこと

- **playgroundでの統合テスト**: compute-agent/VMM(fcvmm/chvmm)/NATS配線/reconciler等、
  実際にVMやリソースが動く経路に関わる変更は、`playground/docker-compose.yml`で該当
  サービスを再ビルドし、実際に動かして確認する（単体テストのpassだけで完了とみなさない）。
  reconcileループを持つ機能はAPI-servingバイナリだけでなく対応する`*-reconciler`
  バイナリの再ビルドも忘れない
  - **例外**: ドキュメントのみの変更、明らかに実行経路に影響しない変更（コメント修正、
    未使用コードの削除等）は省略してよい。厳密な全数適用ルールではなく、
    「影響があるかもしれない変更は必ず実地確認する」という基準で判断する
- **ドキュメントの追従**: 実装を変更したら、影響する範囲で
  [docs/specs/](docs/specs/README.md)（現状の仕様）・[docs/architecture.md](docs/architecture.md)
  （設計判断が変わった場合のみ）・[docs/release-notes.md](docs/release-notes.md)
  （日付付きの変更履歴）を同じPR/コミットの中で更新する。「後で書く」を許さない
  ——コードとドキュメントの乖離は都度の小さな追従コストの方が、まとめて再監査するより
  常に安い

## その他

- `buf generate`前後の手順、playground検証の要否等は各specファイル・`playground/README.md`を参照
