package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/slack-go/slack"

	"github.com/joho/godotenv"
	"github.com/mattn/go-mastodon"
	"github.com/mmcdole/gofeed"
)

const (
	userAgent               = "RSS-Filter-Bot/1.0"
	defaultTimeout          = 10 * time.Second
	mastodonMaxStatusLength = 500
	delayedStartIndex       = 1
)

var (
	s3Bucket            string
	s3ConfigKey         string
	s3StateKey          string
	slackBotToken       string
	mastodonServer      string
	mastodonAccessToken string
)

type Config struct {
	GlobalExcludeWords []string           `json:"global_exclude_keywords"`
	DelayedDomains     []string           `json:"delayed_domains"`
	Configs            []FeedFilterConfig `json:"configs"`
}

type FeedFilterConfig struct {
	Category        string   `json:"category"`
	Description     string   `json:"description"`
	IncludeKeywords []string `json:"include_keywords"`
	ExcludeKeywords []string `json:"exclude_keywords"`
	URLs            []string `json:"urls"`
	SlackChannelID  string   `json:"slack_channel_id"`
	EnableMastodon  bool     `json:"enable_mastodon"`
}

type State struct {
	Feeds map[string]FeedState `json:"feeds"`
}

type FeedState struct {
	LastLink     string `json:"last_link"`
	LastPubDate  string `json:"last_pub_date"`
	LastModified string `json:"last_modified"`
	ETag         string `json:"etag"`
}

type NotificationItem struct {
	Title          string
	Link           string
	Description    string
	PubDate        time.Time
	FeedTitle      string
	Category       string
	SlackChannelID string
	EnableMastodon bool
}

func main() {
	if isLambda() {
		lambda.Start(run)
	} else {
		if err := run(context.Background()); err != nil {
			log.Fatalf("Execution failed: %v", err)
		}
	}
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}

func run(ctx context.Context) error {
	if !isLambda() {
		godotenv.Load()
	}

	loadEnvConfig()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("AWS Config load error: %w", err)
	}
	s3Client := s3.NewFromConfig(cfg)

	appConfig, err := loadAppConfig(ctx, s3Client)
	if err != nil {
		return fmt.Errorf("Config load error: %w", err)
	}

	state, err := loadState(ctx, s3Client)
	if err != nil {
		log.Printf("State load warn (starting fresh): %v", err)
		state = &State{Feeds: make(map[string]FeedState)}
	}

	updatedState, processErr := processFeeds(ctx, appConfig, state)
	if processErr != nil {
		log.Printf("Some feeds failed: %v", processErr)
	}

	if err := saveState(ctx, s3Client, updatedState); err != nil {
		return fmt.Errorf("State save error: %w", err)
	}

	return processErr
}

func loadEnvConfig() {
	s3Bucket = os.Getenv("S3_BUCKET")
	s3ConfigKey = os.Getenv("S3_CONFIG_KEY")
	s3StateKey = os.Getenv("S3_STATE_KEY")
	slackBotToken = os.Getenv("SLACK_BOT_TOKEN")
	mastodonServer = os.Getenv("MASTODON_SERVER")
	mastodonAccessToken = os.Getenv("MASTODON_ACCESS_TOKEN")
}

func loadAppConfig(ctx context.Context, client *s3.Client) (*Config, error) {
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3ConfigKey),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var cfg Config
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func loadState(ctx context.Context, client *s3.Client) (*State, error) {
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3StateKey),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var state State
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func processFeeds(ctx context.Context, appConfig *Config, oldState *State) (*State, error) {
	newState := &State{Feeds: make(map[string]FeedState)}
	for k, v := range oldState.Feeds {
		newState.Feeds[k] = v
	}

	var errorList []string

	for _, feedCfg := range appConfig.Configs {
		excludeWords := append(feedCfg.ExcludeKeywords, appConfig.GlobalExcludeWords...)

		for _, url := range feedCfg.URLs {
			updatedFeedState, err := processSingleFeed(ctx, url, feedCfg, excludeWords, appConfig.DelayedDomains, delayedStartIndex, newState.Feeds)
			if err != nil {
				msg := fmt.Sprintf("Failed to process %s: %v", url, err)
				log.Println(msg)
				errorList = append(errorList, msg)
				continue
			}

			feedKey := fmt.Sprintf("%x", md5.Sum([]byte(url)))
			newState.Feeds[feedKey] = *updatedFeedState
		}
	}

	if len(errorList) > 0 {
		return newState, fmt.Errorf("%d errors encountered", len(errorList))
	}
	return newState, nil
}

func saveState(ctx context.Context, client *s3.Client, state *State) error {
	body, err := json.MarshalIndent(state, "", "    ")
	if err != nil {
		return err
	}

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3StateKey),
		Body:   bytes.NewReader(body),
	})
	return err
}

func processSingleFeed(ctx context.Context, url string, feedCfg FeedFilterConfig, excludeWords []string, delayedDomains []string, delayedStartIndex int, currentFeeds map[string]FeedState) (*FeedState, error) {
	feedKey := fmt.Sprintf("%x", md5.Sum([]byte(url)))
	currentState := currentFeeds[feedKey]

	feed, respHeaders, statusCode, err := fetchAndParse(url, currentState.LastModified, currentState.ETag)
	if err != nil {
		return nil, err
	}

	if statusCode == http.StatusNotModified {
		return &currentState, nil
	}

	nextState := currentState
	nextState.LastModified = respHeaders.Get("Last-Modified")
	nextState.ETag = respHeaders.Get("ETag")

	notifyItems := getNewItemsWithEnrichment(feed.Items, currentState.LastLink, feedCfg.IncludeKeywords, excludeWords, delayedDomains, delayedStartIndex, url)

	notificationCount := 0
	for _, item := range notifyItems {
		nItem := NotificationItem{
			Title:          item.Title,
			Link:           item.Link,
			Description:    item.Description,
			PubDate:        safeParseDate(item.Published),
			FeedTitle:      feed.Title,
			Category:       feedCfg.Category,
			SlackChannelID: feedCfg.SlackChannelID,
			EnableMastodon: feedCfg.EnableMastodon,
		}

		if err := sendNotifications(ctx, nItem); err != nil {
			log.Printf("Notification failed for %s: %v", item.Title, err)
		} else {
			notificationCount++
		}
		time.Sleep(1 * time.Second)
	}

	if len(notifyItems) > 0 {
		mostRecent := notifyItems[0]
		nextState.LastLink = mostRecent.Link
		if mostRecent.Published != "" {
			nextState.LastPubDate = mostRecent.Published
		}
	} else if currentState.LastLink == "" && len(feed.Items) > 0 {
		idx := 0
		if isDelayedDomain(url, delayedDomains) {
			idx = delayedStartIndex
		}
		if idx < len(feed.Items) {
			nextState.LastLink = feed.Items[idx].Link
			nextState.LastPubDate = feed.Items[idx].Published
		}
	}

	return &nextState, nil
}

func fetchAndParse(url string, lastModified, etag string) (*gofeed.Feed, http.Header, int, error) {
	headers := make(map[string]string)
	if lastModified != "" {
		headers["If-Modified-Since"] = lastModified
	}
	if etag != "" {
		headers["If-None-Match"] = etag
	}

	content, respHeaders, statusCode, err := fetchFeedContent(url, headers)
	if err != nil {
		return nil, nil, 0, err
	}
	if statusCode == http.StatusNotModified {
		return nil, respHeaders, statusCode, nil
	}

	fp := gofeed.NewParser()
	feed, err := fp.Parse(bytes.NewReader(content))
	if err != nil {
		return nil, respHeaders, statusCode, fmt.Errorf("parse error: %w", err)
	}

	return feed, respHeaders, statusCode, nil
}

func fetchFeedContent(feedURL string, headers map[string]string) ([]byte, http.Header, int, error) {
	client := &http.Client{Timeout: defaultTimeout}
	req, err := http.NewRequest("GET", feedURL, nil)
	if err != nil {
		return nil, nil, 0, err
	}

	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header, resp.StatusCode, nil
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	return body, resp.Header, resp.StatusCode, err
}

func getNewItemsWithEnrichment(items []*gofeed.Item, lastLink string, include, exclude []string, delayedDomains []string, delayedStartIndex int, feedURL string) []*gofeed.Item {
	var filteredCandidates []*gofeed.Item
	for _, item := range items {
		if passesFilters(item, include, exclude) {
			filteredCandidates = append(filteredCandidates, item)
		}
	}

	startIndex := 0
	isDelayed := isDelayedDomain(feedURL, delayedDomains)
	if isDelayed {
		startIndex = delayedStartIndex
	}

	var notifyItems []*gofeed.Item
	for i := startIndex; i < len(filteredCandidates); i++ {
		item := filteredCandidates[i]

		if item.Link == lastLink {
			break
		}

		if isDelayed {
			if i > 0 {
				item.Published = filteredCandidates[i-1].Published
			}

			if i+1 < len(filteredCandidates) {
				nextEntry := filteredCandidates[i+1]
				item.Description = fmt.Sprintf("%s\n\n前回: <a href=\"%s\">%s</a>", item.Description, nextEntry.Link, nextEntry.Title)
			}
		}

		notifyItems = append(notifyItems, item)
	}
	return notifyItems
}

func passesFilters(item *gofeed.Item, include []string, exclude []string) bool {
	text := strings.ToLower(item.Title + " " + item.Description)

	for _, word := range exclude {
		if strings.Contains(text, strings.ToLower(word)) {
			return false
		}
	}

	if len(include) > 0 {
		found := false
		for _, word := range include {
			if strings.Contains(text, strings.ToLower(word)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func isDelayedDomain(feedURL string, domains []string) bool {
	for _, domain := range domains {
		if strings.Contains(feedURL, domain) {
			return true
		}
	}
	return false
}

func safeParseDate(d string) time.Time {
	layouts := []string{
		time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
		time.RFC3339, "Mon, 02 Jan 2006 15:04:05 -0700",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, d); err == nil {
			return t
		}
	}
	return time.Time{}
}

func sendNotifications(ctx context.Context, item NotificationItem) error {
	var errs []string

	if item.SlackChannelID != "" {
		if err := postToSlack(item); err != nil {
			errs = append(errs, fmt.Sprintf("Slack error: %v", err))
		}
	}

	if item.EnableMastodon {
		if err := postToMastodon(ctx, item); err != nil {
			errs = append(errs, fmt.Sprintf("Mastodon error: %v", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, ", "))
	}
	return nil
}

func postToSlack(item NotificationItem) error {
	api := slack.New(slackBotToken)

	msgText := fmt.Sprintf("*%s*\n%s\nCategory: %s", item.Title, item.Link, item.Category)

	_, _, err := api.PostMessage(
		item.SlackChannelID,
		slack.MsgOptionText(msgText, false),
	)
	return err
}

func postToMastodon(ctx context.Context, item NotificationItem) error {
	c := mastodon.NewClient(&mastodon.Config{
		Server:      mastodonServer,
		AccessToken: mastodonAccessToken,
	})

	status := fmt.Sprintf("%s\n%s #%s", item.Title, item.Link, item.Category)
	runes := []rune(status)
	if len(runes) > mastodonMaxStatusLength {
		status = string(runes[:mastodonMaxStatusLength-3]) + "..."
	}

	_, err := c.PostStatus(ctx, &mastodon.Toot{
		Status:     status,
		Visibility: mastodon.VisibilityUnlisted,
	})
	return err
}
