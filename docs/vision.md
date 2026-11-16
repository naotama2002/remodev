# remodev ビジョン

> このドキュメントは常に最新に保つ。設計や方針を変える PR では、同じ PR でここも更新する（ロードマップのチェックも含む）。

remodev は、チームのプロダクト開発を支えるセルフホスト型の Background Coding Agent 基盤です。
LayerX「haro」を主な目標像とし、Ubie「Remoco / unaba」、GMO Flatt Security「Sunaba」の考え方を取り入れます。

画面のモック: [docs/mocks/index.html](mocks/index.html)（静的 HTML。ブラウザで開くと全画面を切り替えて見られる）

![セッション詳細](mocks/detail.png)

## 1. 目指す姿

> 「コードを読み書きでき、動作確認もできる環境」を、誰でも・どこからでも・すぐに払い出せる。
> エージェントには確認なしで作業させ、その安全は仕組みで担保する。

- Web / Slack / 定期実行 / API から依頼すると、必要なものが揃った隔離環境で Claude Code が動き、
  実装 → 動作確認 → スクリーンショット → PR まで手放しで進む。
- セッションは原則チームに公開され、隣の人の上手な使い方を真似できる。
- 蓄積したセッションのログから、prompt や skill が自動で改善されていく（ループが閉じる）。

## 2. 設計原則

1. **サンドボックスに秘密を置かない。** 本物の認証情報は egress プロキシ（nono）だけが持ち、許可した宛先にだけ注入する。
2. **外向き通信はデフォルト拒否。** プロキシ以外に出口はない。GitHub への書き込みはセッションの対象リポジトリだけ。
3. **だから YOLO で動かせる。** 細かい承認を挟まず、寝ている間も進む。
4. **ロジックは Go、TS は表示だけ。** 依存は最小、ダウンロードするバイナリはバージョンとハッシュで固定。
5. **ランニングコストは最小。** 使っていない環境は止め、作業領域は期限で消す。ログは残す。
6. **手元でも AWS でも同じものが動く。** `docker compose up` で全機能を試せる。

## 3. 参考記事からの対応表

| 要素 | haro / Remoco / Sunaba | remodev |
|---|---|---|
| 起動経路 | Web, Slack, Scheduler, API | 同じ（Web は済） |
| 隔離 | Pod (EKS) / Kata microVM / Firecracker | Docker + internal network。オプションで gVisor / Kata |
| 認証情報 | プロキシで注入（Remoco） | nono で注入（済） |
| Agent Loop | Temporal Workflow | Go のジョブ管理 + Claude Code の `--resume` |
| 環境定義 | 社内 monorepo 向けに作り込み | `.remodev/` または devcontainer.json |
| 動作確認 | Android エミュレータ、スクショ | Playwright + 成果物保存（エミュレータは後回し） |
| 外部連携 | Slack, Notion, Linear, Datadog の MCP | MCP + 個人 OAuth を nono で注入 |
| カスタム Agent | prompt / MCP / 環境 のプリセット | 同じ |
| 公開範囲 | 原則公開、機密は制限 | 同じ。個人権限セッションは非公開 |
| 改善ループ | セッションを分析して改善 | 毎日分析して改善 PR を作る Agent |

## 4. 画面

| 画面 | 内容 |
|---|---|
| セッション一覧 | チーム全員のセッション。リポジトリ・作成者・Agent・状態で絞り込み |
| セッション詳細 | 中央にタイムライン、右に成果物パネル（スクショ・HTML・差分・プレビュー・PR/CI）、下に入力欄。URL で共有 |
| 新規セッション | リポジトリ・Agent・ベースブランチを選び指示を書く |
| Agent カタログ | カスタム Agent の一覧・作成・編集（公開） |
| スケジュール | cron で Agent を起動、結果を Slack に投稿 |
| 連携 | 個人ごとの OAuth 接続（GitHub, Notion, Linear …） |
| ダッシュボード | remodev 経由 PR の割合、利用者数、セッション数、コスト |

## 5. アーキテクチャ（最終形）

```
 Web UI ─┐                         ┌─ store: SQLite (手元) / DynamoDB or RDS (AWS)
 Slack  ─┼─► control (Go) ─────────┼─ artifacts: volume (手元) / S3 (AWS)
 cron   ─┤    ├ session manager    └─ secrets: env (手元) / Secrets Manager (AWS)
 API    ─┘    ├ scheduler
              ├ runtime: docker (手元・EC2) | ecs (将来)
              └ policy: nono profile 生成
                     │
     ┌───────── per session, internal network ─────────┐
     │ sandbox (repo 環境 + Claude Code + Playwright)   │
     │        └─ HTTPS_PROXY ─► nono ──────────────────┼─► GitHub / Anthropic / MCP / registries
     └──────────────────────────────────────────────────┘
```

AWS（低コスト構成の案）:
- control と sandbox を **EC2 スポット 1 台**（足りなくなったら Auto Scaling で台数を増やす）で動かす。
- 入口は ALB + OIDC（GitHub / Google）、プレビューはワイルドカード DNS。
- 作業領域は EBS、成果物とログのアーカイブは S3、秘密情報は Secrets Manager。
- 夜間や休日はインスタンスを 0 台にできる構成にする。

### 環境定義

リポジトリの `.remodev/` ディレクトリでサンドボックスの環境を定義する。

| ファイル | 役割 |
|---|---|
| `.remodev/Dockerfile` | `ARG REMODEV_BASE` / `FROM ${REMODEV_BASE}` で標準サンドボックスを拡張し、言語やツールを足す。ビルドは control が Docker で行う（プロキシを通らない。ダウンロードは sha256 で固定すること） |
| `.remodev/setup.sh` | 最初の clone 後にサンドボックス内で一度だけ実行（依存のインストールなど）。プロキシ経由で動く |
| その他 | Dockerfile のビルドコンテキストとして使える |

- `.remodev/` はセッションのベースブランチから GitHub API で読む（最大 64 ファイル、各 1MB）。
- イメージ名は `remodev-env:<hash>`。hash は標準イメージの ID と `.remodev/` の全ファイル内容から作るので、どちらかが変われば作り直す。同じなら再利用する。
- `.remodev/Dockerfile` が無いリポジトリは標準イメージ（Claude Code, gh, node, python）で動く。
- 実測（remodev 自身、Go を追加）: 初回ビルド込み 85 秒、キャッシュ利用時 40 秒（大半は setup.sh の `go build`）。

**方針（最終形）: 実行時にビルドしない。ECR に登録済みのイメージだけを使う。**

- 理由: セッションのたびにビルドすると、ビルド時のダウンロード（プロキシを通らない）が毎回走り、何が入ったイメージで動いたかも追いにくい。セキュリティ上、実行環境は事前に検証・登録したものに限定したい。
- 形: `.remodev/Dockerfile` は CI（GitHub Actions）でビルドし、スキャン後に ECR へ push する。control はリポジトリ/チームに登録されたイメージ（digest 固定）を pull して使うだけにする。未登録なら標準イメージで動かす。
- 現状（手元・開発段階）: control がその場でビルドし、内容ハッシュでキャッシュする。ビルドがプロキシを通らないのは想定どおりとして許容する。ECR 方式に移ったら、本番では実行時ビルドを無効にする（手元では残す）。

### 成果物と動作確認

- サンドボックスには Debian の Chromium と playwright-core（依存なし・ブラウザのダウンロードなし、npm の sha512 で固定）を入れる。Playwright 本体（テストランナー）は入れない。
- `remodev-screenshot <url> [name]` で全画面の PNG を `/workspace/artifacts/` に保存する。複雑な操作はエージェントが playwright-core のスクリプトを書く。外部 URL はプロキシ経由、localhost は直接。
- `/workspace/artifacts/` のファイルは、ターン中 10 秒ごとと終了時に control が取り出し、`<data>/artifacts/<session>/` に保存して `artifact` イベントを出す（1 ファイル 20MB、1 セッション 200 個まで）。サンドボックスや作業領域を消しても残る。
- 成果物はエージェントが作ったもの＝信用しないデータとして配信する: `Content-Security-Policy: sandbox`、`nosniff`、ファイル名は平坦な英数字のみ。
- 画面右のパネル: 成果物（PR / CI、スクショ、その他ファイル）、差分（作業中はサンドボックスの `git diff`、停止後は GitHub の compare）、プレビュー。

### 隔離方式の判断

Claude Code が動く場所は「EC2 上の Docker コンテナ（1 セッション = sandbox + nono の 1 組）」とする。作業者ごとに EC2 は割り当てない。

| 方式 | 起動 | コスト | 隔離 | 判断 |
|---|---|---|---|---|
| EC2 上の Docker コンテナ | 数秒（実測 3〜4s、再開 2.6s） | 最安（相乗り、0 台まで縮小可） | コンテナ（カーネル共有） | **当面これ**（haro の EKS Pod と同水準） |
| 同上 + gVisor (`runsc`) / Kata | ほぼ同じ | ほぼ同じ | 強い（Kata は nested virt が必要） | 公開範囲を広げる・外部コードを動かすときに有効化（`REMODEV_SANDBOX_RUNTIME`） |
| ECS Fargate 1 セッション 1 タスク | 30 秒〜数分 | 中 | microVM | さらに強い隔離が必要になったら Runtime を追加 |
| 1 セッション 1 EC2 | 1〜2 分 | 高 | VM | 採用しない（Flatt が遅すぎるとして避けた方式） |

残るリスク: 同一ホストのセッション間はカーネルを共有するため、コンテナエスケープ時に他セッションの作業領域が見える可能性がある。認証情報はサンドボックスに無く外向き通信も制限しているため、盗める秘密は限定的。

## 6. ロードマップ

### Phase 1: 実際の開発に使える（いまここ）
- [x] 隔離 + nono による認証情報注入
- [x] Web UI、ログのリアルタイム配信、PR 作成
- [x] 環境定義: `.remodev/Dockerfile` + `.remodev/setup.sh`、内容ハッシュでイメージをキャッシュ（[§5 環境定義](#環境定義)）
- [ ] 環境定義の高速化: ビルドキャッシュ（Go / npm など）の共有ボリューム、devcontainer.json 対応、古い env イメージの掃除
- [ ] 環境イメージを CI でビルド → スキャン → ECR 登録し、control は登録済みイメージ（digest 固定）のみ使用。本番では実行時ビルドを無効化
- [x] ブラウザでの動作確認とスクショ（Chromium + playwright-core、`remodev-screenshot`）、成果物パネル（[§5 成果物](#成果物と動作確認)）
- [x] 差分の表示（作業中はサンドボックスの未コミットを含む、停止後は GitHub）、PR / CI 状態の表示
- [ ] 成果物を S3 に保存（AWS 構成と同時）、PR 本文へのスクショ添付

### 品質の土台
- [x] テスト: Go（fake の Runtime / GitHub / Claude で Manager と HTTP API を振る舞いテスト、-race）、Web（純粋ロジックの単体テスト + 型チェック）、ブラウザ E2E（fake モードの本物のバイナリを Chromium で操作）
- [x] CI（GitHub Actions、アクションは SHA で固定）
- [x] `REMODEV_RUNTIME=fake`: 認証情報なしで control と UI を動かすモード（UI 開発と E2E 用）

### Phase 2: チームで使える
- [ ] GitHub ログイン、org / ユーザーでの制限
- [ ] セッションの公開範囲（公開 / 非公開）
- [ ] Slack（スレッドから起動、結果をスレッドに返信、追加の指示）
- [ ] AWS 構成（Terraform）
- [ ] GitHub App のトークン（リポジトリ単位、短命）

### Phase 3: haro らしさ
- [ ] Agent カタログ（UI から作成・編集）
- [ ] MCP 接続と個人 OAuth（nono で注入、個人セッションは非公開）
- [ ] スケジュール実行、HTTP API からの起動
- [ ] HTML 成果物へのコメント → AI へのフィードバック

### Phase 4: ループを閉じる
- [ ] ダッシュボード（PR 割合・利用者・セッション・コスト）
- [ ] 前日のセッションを分析し、prompt / skill の改善 PR を出す Agent
- [ ] 実行中ターンの耐久性（control 再起動をまたいで継続）

## 7. やらないこと（当面）

- 自前の microVM 基盤（Firecracker / Sunaba 相当）。必要になったら Kata / gVisor から試す。
- Temporal の導入。ジョブ表で足りなくなったら再検討する。
- Android エミュレータ（KVM が使える metal インスタンスが必要なため）。
