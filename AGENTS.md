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
- テスト: Go がある環境では `go test ./...`。ホストに Go が無いときは `make test`（Docker の golang イメージで実行）。
  - remodev のサンドボックスでは `.remodev/Dockerfile` で Go が入っているので `go test ./...` を使う（Docker は無い）。
- サンドボックス環境を変えたら `.remodev/` を更新する（docs/vision.md の「環境定義」参照）。
- ダウンロードするバイナリとベースイメージはバージョンと sha256 で固定する。
