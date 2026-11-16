# AGENTS.md

remodev のリポジトリで作業する人とエージェント向けのルール。

## 設計ドキュメント
- `docs/vision.md` が設計の正。常に最新に保つ。
- 設計・方針・構成・ロードマップに影響する変更は、**同じ PR で `docs/vision.md` も更新する**。
  - 実装が終わった項目はロードマップのチェックを付ける。
  - 方針を変えたら、理由も一緒に書く（例: 「隔離方式の判断」の表）。
- 画面を変えたら必要に応じて `docs/mocks/` も更新する。

## 開発
- main は PR 経由のみ。ブランチを切って PR を作る。
- Go は標準ライブラリのみ。外部依存を足すときは PR で理由を書く。
- `web/` は表示だけ。ロジックは Go に置く。依存は exact version、install script 無効。
  - `web/dist/.gitkeep` は `go build`（embed）に必要。vite の出力で消えないよう `web/public/.gitkeep` から複製される。消さないこと。
- テスト（変更したら必ず実行し、壊さない。CI でも同じものが走る）:
  - `make test` = `test-go`（Go, -race）+ `test-web`（型チェック + `web/src/*.test.ts`）+ `test-e2e`（ブラウザ E2E）。ホストに必要なのは Docker だけ。
  - サンドボックス内（Go あり・Docker なし）では `go test ./...` と `cd web && pnpm typecheck && pnpm test`。
- テストの方針（テストがすぐ壊れるなら構造が悪いと考える）:
  - 振る舞いをテストする。Go は `internal/testenv`（fake の Runtime / GitHub / Claude で組んだ本物の Manager + HTTP サーバー）を通して API とイベントを検証する。内部関数や private な状態に依存しない。
  - 外部依存（Docker・GitHub・Claude）は `session.Runtime` / `session.GitHub` のインターフェース越しにだけ使い、`internal/fake` で置き換える。新しい依存も同じ形にする。
  - UI のロジックは `web/src/lib.ts`（React/DOM なし）に出して単体テストする。コンポーネントには業務ロジックを書かない。
  - E2E（`web/e2e/`）は `REMODEV_RUNTIME=fake` の本物のバイナリをブラウザで操作し、ユーザーに見えるもの（role / label / 表示テキスト）で検証する。CSS クラスに依存しない。未捕捉の JS エラーは失敗にする。
  - 不具合を直すときは、先にそれを再現するテストを書く。
- `REMODEV_RUNTIME=fake`（`make fake`）で、認証情報なしに UI を動かせる。
- サンドボックス環境を変えたら `.remodev/` を更新する（docs/vision.md の「環境定義」参照）。
- ダウンロードするバイナリとベースイメージはバージョンと sha256 で固定する。
