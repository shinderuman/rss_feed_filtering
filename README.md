# RSS Feed Filtering Service

A serverless RSS feed aggregation and filtering service built with Go and AWS Lambda. This service fetches multiple RSS feeds, applies keyword-based filtering, and generates customized RSS feeds based on configurable categories.

## Features

- **Multi-source RSS aggregation**: Combine multiple RSS feeds into a single filtered feed
- **Keyword filtering**: Include/exclude articles based on configurable keywords
- **Category-based configuration**: Organize feeds by categories with different filtering rules
- **Delayed publishing**: Configure delays for specific domains to control content timing
- **AWS Lambda deployment**: Serverless architecture with automatic scaling
- **Local development support**: Run and test locally before deployment

## Architecture

- **Runtime**: Go 1.24.2
- **Deployment**: AWS Lambda with Function URL
- **Configuration**: JSON config stored in S3
- **Dependencies**: AWS SDK v2, gofeed parser

## Quick Start

### Prerequisites

- Go 1.24.2 or later
- AWS CLI configured with appropriate permissions
- jq (for deployment script)

### Local Development

1. Clone the repository
2. Install dependencies:
   ```bash
   go mod download
   ```

3. Create configuration file in S3 (see Configuration section)

4. Run locally:
   ```bash
   go run main.go <category>
   ```

### Deployment

Deploy to AWS Lambda using the provided script:

```bash
# Build and deploy
./deploy.sh

# Build only (for testing)
./deploy.sh --build-only
```

## Configuration

Create a `config.json` file in your S3 bucket with the following structure:

```json
{
    "global_exclude_keywords": ["spam", "ads"],
    "delayed_domains": ["news-site.invalid"],
    "delay_days": 7,
    "configs": [
        {
            "category": "tech",
            "description": "Technology News",
            "include_keywords": ["programming", "software"],
            "exclude_keywords": ["gossip"],
            "urls": [
                "https://example.invalid/tech-feed.xml",
                "https://another-site.invalid/rss"
            ]
        }
    ]
}
```

### Configuration Options

- `global_exclude_keywords`: Keywords to exclude across all categories
- `delayed_domains`: Domains that require publication delay
- `delay_days`: Number of days to delay publication for delayed domains
- `configs`: Array of category configurations

## API Usage

Access the deployed Lambda function via its Function URL:

```
https://your-function-url.example.invalid/?category=tech&token=your-access-token
```

## Security

- Access token authentication required for all requests
- S3 bucket permissions for configuration access
- Lambda execution role with minimal required permissions

---

# RSS フィード フィルタリング サービス

Go と AWS Lambda で構築されたサーバーレス RSS フィード集約・フィルタリングサービスです。複数の RSS フィードを取得し、キーワードベースのフィルタリングを適用して、設定可能なカテゴリに基づいてカスタマイズされた RSS フィードを生成します。

## 機能

- **マルチソース RSS 集約**: 複数の RSS フィードを単一のフィルタリングされたフィードに統合
- **キーワードフィルタリング**: 設定可能なキーワードに基づいて記事を含める/除外する
- **カテゴリベース設定**: 異なるフィルタリングルールでフィードをカテゴリ別に整理
- **遅延公開**: 特定のドメインに対して遅延を設定してコンテンツのタイミングを制御
- **AWS Lambda デプロイメント**: 自動スケーリング機能付きサーバーレスアーキテクチャ
- **ローカル開発サポート**: デプロイ前にローカルで実行・テスト可能

## アーキテクチャ

- **ランタイム**: Go 1.24.2
- **デプロイメント**: Function URL 付き AWS Lambda
- **設定**: S3 に保存された JSON 設定ファイル
- **依存関係**: AWS SDK v2、gofeed パーサー

## クイックスタート

### 前提条件

- Go 1.24.2 以降
- 適切な権限で設定された AWS CLI
- jq（デプロイスクリプト用）

### ローカル開発

1. リポジトリをクローン
2. 依存関係をインストール:
   ```bash
   go mod download
   ```

3. S3 に設定ファイルを作成（設定セクションを参照）

4. ローカルで実行:
   ```bash
   go run main.go <カテゴリ>
   ```

### デプロイメント

提供されたスクリプトを使用して AWS Lambda にデプロイ:

```bash
# ビルドとデプロイ
./deploy.sh

# ビルドのみ（テスト用）
./deploy.sh --build-only
```

## 設定

S3 バケットに以下の構造で `config.json` ファイルを作成:

```json
{
    "global_exclude_keywords": ["スパム", "広告"],
    "delayed_domains": ["news-site.invalid"],
    "delay_days": 7,
    "configs": [
        {
            "category": "tech",
            "description": "テクノロジーニュース",
            "include_keywords": ["プログラミング", "ソフトウェア"],
            "exclude_keywords": ["ゴシップ"],
            "urls": [
                "https://example.invalid/tech-feed.xml",
                "https://another-site.invalid/rss"
            ]
        }
    ]
}
```

### 設定オプション

- `global_exclude_keywords`: すべてのカテゴリで除外するキーワード
- `delayed_domains`: 公開遅延が必要なドメイン
- `delay_days`: 遅延ドメインの公開を遅らせる日数
- `configs`: カテゴリ設定の配列

## API 使用方法

Function URL 経由でデプロイされた Lambda 関数にアクセス:

```
https://your-function-url.example.invalid/?category=tech&token=your-access-token
```

## セキュリティ

- すべてのリクエストにアクセストークン認証が必要
- 設定アクセス用の S3 バケット権限
- 最小限の必要権限を持つ Lambda 実行ロール