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
            "category": "tech",
            "description": "技術ニュース",
            "urls": ["https://example.com/feed.xml"],
            "slack_channel_id": "C0123456789",
            "enable_mastodon": true,
            "exclude_keywords": ["ゴシップ"]
        }
    ]
}
```

- **delayed_domains**: ここに含まれるドメインのフィードは、常に「最新の1つ前」の記事から処理が開始されます（コード内で `delayedStartIndex = 1` 固定）。