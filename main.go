package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mmcdole/gofeed"
)

const (
	s3Bucket       = "rss-feed-filtering"
	s3Key          = "config.json"
	accessTokenKey = "token"
	accessTokenVal = "shoh7Ahghoongiez3PhuYiungie3XaiphooVooquai3daishie"
)

var pubDateLayouts = []string{
	time.RFC1123Z,                     // "Mon, 02 Jan 2006 15:04:05 -0700"
	time.RFC1123,                      // "Mon, 02 Jan 2006 15:04:05 MST"
	time.RFC822Z,                      // "02 Jan 06 15:04 -0700"
	time.RFC822,                       // "02 Jan 06 15:04 MST"
	time.RFC3339,                      // "2006-01-02T15:04:05Z07:00"
	"Mon, 02 Jan 2006 15:04:05 -0700", // 明示的なパターン
	"Mon, 02 Jan 2006 15:04:05 MST",
	"2006-01-02 15:04:05",
	"02 Jan 2006 15:04:05",
}

type FeedFilterConfig struct {
	Category        string   `json:"category"`
	Description     string   `json:"description"`
	IncludeKeywords []string `json:"include_keywords"`
	ExcludeKeywords []string `json:"exclude_keywords"`
	URLs            []string `json:"urls"`
}

type RSSFeed struct {
	XMLName xml.Name     `xml:"rss"`
	Version string       `xml:"version,attr"`
	Channel RSSFeedItems `xml:"channel"`
}

type RSSFeedItems struct {
	Title       string        `xml:"title"`
	Description string        `xml:"description"`
	Link        string        `xml:"link"`
	Items       []RSSFeedItem `xml:"item"`
}

type RSSFeedItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}

func main() {
	if isLambda() {
		lambda.Start(lambdaHandler)
	} else {
		if err := runLocal(); err != nil {
			fmt.Fprintln(os.Stderr, "エラー:", err)
			os.Exit(1)
		}
	}
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}

func lambdaHandler(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	if req.QueryStringParameters[accessTokenKey] != accessTokenVal {
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusUnauthorized,
			Body:       "Unauthorized: トークンが不正です",
		}, nil
	}

	config, err := fetchFeedFilterConfig(ctx, req.QueryStringParameters["category"])
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusBadRequest,
			Body:       "カテゴリの指定が不正です: " + err.Error(),
		}, nil
	}

	rssXML, err := generateRSS(*config)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       "RSS生成エラー: " + err.Error(),
		}, nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "application/rss+xml"},
		Body:       rssXML,
	}, nil
}

func runLocal() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("カテゴリを指定してください（例: go run main.go games）")
	}

	config, err := fetchFeedFilterConfig(context.Background(), os.Args[1])
	if err != nil {
		return err
	}

	rssXML, err := generateRSS(*config)
	if err != nil {
		return err
	}

	fmt.Println(rssXML)
	return nil
}

func fetchFeedFilterConfig(ctx context.Context, categoryName string) (*FeedFilterConfig, error) {
	filterConfigs, err := loadFilterConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("設定の読み込み失敗: %w", err)
	}

	for _, cfg := range filterConfigs {
		if cfg.Category == categoryName {
			return &cfg, nil
		}
	}
	return nil, fmt.Errorf("カテゴリ '%s' が見つかりません", categoryName)
}

func loadFilterConfigs(ctx context.Context) ([]FeedFilterConfig, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg)
	output, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		return nil, err
	}
	defer output.Body.Close()

	var configs []FeedFilterConfig
	if err := json.NewDecoder(output.Body).Decode(&configs); err != nil {
		return nil, err
	}
	return configs, nil
}

func generateRSS(cfg FeedFilterConfig) (string, error) {
	fp := gofeed.NewParser()
	var items []RSSFeedItem

	for _, url := range cfg.URLs {
		feed, err := fp.ParseURL(url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "RSS取得失敗 [%s]: %v\n", url, err)
			continue
			// return "", err
		}

		for _, entry := range feed.Items {
			if !passesFilters(entry, cfg) {
				continue
			}

			items = append(items, RSSFeedItem{
				Title:       fmt.Sprintf("[%s] %s", feed.Title, entry.Title),
				Link:        entry.Link,
				Description: entry.Description,
				PubDate:     entry.Published,
			})
		}
	}

	sort.Slice(items, func(i, j int) bool {
		ti := parsePubDate(items[i].PubDate)
		tj := parsePubDate(items[j].PubDate)
		return ti.After(tj)
	})

	rss := RSSFeed{
		Version: "2.0",
		Channel: RSSFeedItems{
			Title:       cfg.Description,
			Description: cfg.Description,
			Link:        "https://example.com",
			Items:       items,
		},
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	encoder := xml.NewEncoder(&buf)
	encoder.Indent("", "  ")
	if err := encoder.Encode(rss); err != nil {
		return "", fmt.Errorf("RSSエンコード失敗: %w", err)
	}

	return buf.String(), nil
}

func passesFilters(entry *gofeed.Item, cfg FeedFilterConfig) bool {
	textLower := strings.ToLower(entry.Title + entry.Description)
	if len(cfg.IncludeKeywords) > 0 {
		found := false
		for _, word := range cfg.IncludeKeywords {
			if strings.Contains(textLower, strings.ToLower(word)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	for _, word := range cfg.ExcludeKeywords {
		if strings.Contains(textLower, strings.ToLower(word)) {
			return false
		}
	}

	return true
}

func parsePubDate(s string) time.Time {
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	// 失敗したら最も古い日付を返す（= 一番後ろにされる）
	log.Printf("日付パース失敗: %s", s)
	return time.Time{}
}
