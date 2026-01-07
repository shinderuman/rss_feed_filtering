# RSS Feed Filtering & Notification Service (Push Architecture)

GoとAWS Lambdaで構築されたサーバーレスRSS通知サービスです。
指定されたRSSフィードを定期的に取得し、フィルタリング条件（キーワード除外・遅延配信など）を適用した後、新しい記事をSlackやMastodonに通知します。

## 主な機能

- **プッシュ型通知**: SlackおよびMastodonへの自動投稿に対応。
- **フィルタリング**:
  - 全体除外キーワード（`global_exclude_keywords`）
  - カテゴリごとの除外/必須キーワード設定
- **Delayed Notification（遅延配信）**:
  - 特定のドメイン（例: コミック配信サイト等）に対し、常に「1件古い記事」を通知するロジックを搭載。
  - 最新記事が有料で、1つ前の記事が無料公開されるようなケースに対応（例: `delayedStartIndex = 1`）。
- **状態管理**:
  - S3上の `state.json` に各フィードの最終通知位置（LastLink）や更新日時（LastPubDate）を保存。
  - 重複通知を防止し、効率的な差分チェックを実現（ETag/Last-Modified対応）。
- **自動翻訳**:
  - Anthropic API (Claude) を使用して、フィードのタイトルと説明を日本語に翻訳・要約可能。
  - フィードごとに有効/無効を設定可能。
- **サーバーレス**: AWS Lambda上で動作し、イベントブリッジ等で定期実行することを想定。

## 構成

- **言語**: Go 1.24.2
- **インフラ**: AWS Lambda (provided.al2023)
- **設定・状態**: AWS S3 (`config.json`, `state.json`)

## 必要要件

- Go 1.24.2+
- AWS CLI (設定済みであること)
- 各種トークン（Slack Bot Token, Mastodon Access Token）

## 設定 (Environment Variables)

本番環境（Lambda）およびローカル実行時に以下の環境変数が必要です。

| 変数名 | 説明 | 例 |
|---|---|---|
| `S3_BUCKET` | 設定/状態ファイルを保存するS3バケット名 | `universal-lambda-config` |
| `S3_CONFIG_KEY` | 設定ファイルのS3キー | `rss/config.json` |
| `S3_STATE_KEY` | 状態ファイルのS3キー | `rss/state.json` |
| `SLACK_BOT_TOKEN` | Slack BotのOAuthトークン | `xoxb-...` |
| `MASTODON_SERVER` | MastodonインスタンスURL | `https://mstdn.example.com/` |
| `MASTODON_ACCESS_TOKEN` | Mastodonアクセストークン | `...` |
| `ANTHROPIC_AUTH_TOKEN` | Anthropic APIキー | `your_anthropic_api_key` |
| `ANTHROPIC_BASE_URL` | Anthropic APIベースURL | `https://api.z.ai/api/anthropic` |
| `ANTHROPIC_DEFAULT_MODEL` | 使用するモデルID | `glm-4.6` |

## ローカル開発

### 1. セットアップ

```bash
git clone <repository>
cd rss
go mod download
```

### 2. 環境変数の準備

プロジェクトルートに `.env` ファイルを作成します（`.env.example` を参考にしてください）。

```bash
cp .env.example .env
vi .env
# 各値を設定
```

### 3. 実行

```bash
go run main.go
```

`.env` ファイルが読み込まれ、S3から設定を取得し、RSS取得・フィルタリング・通知（実際に送信されます）が実行されます。

## デプロイ

AWS Lambdaへのデプロイには付属のスクリプトを使用します。

```bash
./deploy.sh
```

- ビルド (`bootstrap` バイナリ作成)
- ZIP圧縮
- AWS Lambdaへのコード更新 (`aws lambda update-function-code`)

が行われます。

## 設定ファイル仕様 (config.json)

S3に配置するJSON設定ファイルの形式です。

```json
{
    "global_exclude_keywords": ["PR", "広告"],
    "delayed_domains": ["https://kimicomi.com"],
    "configs": [
        {
            "include_keywords": [
                "技術"
            ],
            "exclude_keywords": [
                "ゴシップ"
            ],
            "urls": ["https://example.com/feed.xml"],
            "slack_channel_id": "C0123456789",
            "enable_mastodon": true,
            "enable_translation": true
        }
    ]
}
```


### ルート設定項目

| 項目名 | 型 | 必須 | 説明 |
|---|---|---|---|
| `global_exclude_keywords` | []string | 任意 | 全フィードに適用される除外キーワード。 |
| `delayed_domains` | []string | 任意 | 遅延配信（常に1つ前の記事を対象とする）を適用するドメインのリスト。 |
| `configs` | []FeedFilterConfig | **必須** | フィードごとの設定リスト（詳細は後述）。 |


### フィード設定項目 (`configs` 配列内)

| 項目名 | 型 | 必須 | 説明 |
|---|---|---|---|
| `urls` | []string | **必須** | RSSフィードのURLリスト。 |
| `slack_channel_id` | string | 任意 | 通知先のSlackチャンネルID（`C...`）。未設定の場合はSlack通知が行われません。 |
| `enable_mastodon` | bool | 任意 | `true` の場合、Mastodonへの投稿を行います（要設定）。 |
| `enable_translation` | bool | 任意 | `true` の場合、Anthropic APIを使用してタイトルと説明を日本語に翻訳します。 |
| `include_keywords` | []string | 任意 | 必須キーワード。設定されている場合、これらのキーワードの**いずれか**を含む記事のみ通知されます（OR条件）。 |
| `exclude_keywords` | []string | 任意 | 除外キーワード。設定されている場合、これらのキーワードの**いずれか**を含むの記事は通知から除外されます（最優先）。 |