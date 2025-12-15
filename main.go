package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/slack-go/slack"

	"github.com/joho/godotenv"
	"github.com/mattn/go-mastodon"
	"github.com/mmcdole/gofeed"

	"github.com/PuerkitoBio/goquery"
)

const (
	userAgent                = "RSS-Filter-Bot/1.0"
	defaultTimeout           = 10 * time.Second
	mastodonMaxStatusLength  = 500
	delayedStartIndex        = 1
	domainDelay              = 500 * time.Millisecond
	notificationDelay        = 200 * time.Millisecond // Initial notification limit for new feeds
	initialNotificationLimit = 5

	// 1: FeedTitle, 2: Title, 3: Link, 4: Description, 5: PreviousTitle, 6: PreviousLink
	slackFormatDelayed = `*<%[6]s|[%[1]s] %[5]s>*

*<%[3]s|[%[1]s] %[2]s>*

%[4]s`

	slackFormatNormal = `*<%[3]s|[%[1]s] %[2]s>*

%[4]s`

	mastodonFormatDelayed = `[%[1]s]

%[5]s
%[6]s

%[2]s
%[3]s

%[4]s`

	mastodonFormatNormal = `[%[1]s] %[2]s
%[3]s

%[4]s`
)

var (
	s3Bucket            string
	s3ConfigKey         string
	s3StateKey          string
	slackBotToken       string
	mastodonServer      string
	mastodonAccessToken string
	urlRegex            = regexp.MustCompile(`https?://[^\s]+`)
)

type Config struct {
	GlobalExcludeWords []string           `json:"global_exclude_keywords"`
	DelayedDomains     []string           `json:"delayed_domains"`
	Configs            []FeedFilterConfig `json:"configs"`
}

type FeedFilterConfig struct {
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
	LastModified string `json:"last_modified"`
	ETag         string `json:"etag"`
}

type NotificationItem struct {
	Title          string
	Link           string
	Description    string
	FeedTitle      string
	SlackChannelID string
	EnableMastodon bool
	PreviousTitle  string
	PreviousLink   string
}

func (n NotificationItem) Format(forSlack bool) string {
	d := mastodonFormatDelayed
	nm := mastodonFormatNormal
	if forSlack {
		d = slackFormatDelayed
		nm = slackFormatNormal
	}

	escape := func(text string) string {
		replacer := strings.NewReplacer(
			"&", "&amp;",
			"<", "&lt;",
			">", "&gt;",
			"*", "∗",
			"_", "＿",
			"~", "～",
			"`", "｀",
		)
		return replacer.Replace(text)
	}

	format := nm
	if n.PreviousLink != "" {
		format = d
	}

	return fmt.Sprintf(format,
		escape(n.FeedTitle),
		escape(n.Title),
		n.Link,
		n.Description,
		escape(n.PreviousTitle),
		n.PreviousLink,
	)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if isLambda() {
		lambda.Start(run)
	} else {
		if err := run(context.Background()); err != nil {
			slog.Error("Execution failed", "error", err)
			os.Exit(1)
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
		return fmt.Errorf("State load error: %w", err)
	}

	updatedState, processErr := processFeeds(ctx, appConfig, state)
	if processErr != nil {
		slog.Error("Some feeds failed", "error", processErr)
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
		var noKey *types.NoSuchKey
		if errors.As(err, &noKey) {
			slog.Info("State file not found, starting fresh")
			return &State{Feeds: make(map[string]FeedState)}, nil
		}
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
	maps.Copy(newState.Feeds, oldState.Feeds)

	for _, feedCfg := range appConfig.Configs {
		excludeWords := append(feedCfg.ExcludeKeywords, appConfig.GlobalExcludeWords...)

		sort.Strings(feedCfg.URLs)

		var lastHostname string
		for i, url := range feedCfg.URLs {
			currentHostname := getHostname(url)
			if i > 0 && currentHostname == lastHostname && currentHostname != "" {
				time.Sleep(domainDelay)
			}
			lastHostname = currentHostname

			feedKey := fmt.Sprintf("%x", md5.Sum([]byte(url)))
			currentState := newState.Feeds[feedKey]

			updatedFeedState, err := processSingleFeed(ctx, url, feedCfg, excludeWords, appConfig.DelayedDomains, currentState)
			if err != nil {
				slog.Error("Failed to process feed",
					"error_type", "fetch_error",
					"url", url,
					"error", err,
				)
				continue
			}

			newState.Feeds[feedKey] = *updatedFeedState
		}
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

func getHostname(urlStr string) string {
	u, err := url.Parse(urlStr)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func processSingleFeed(ctx context.Context, url string, feedCfg FeedFilterConfig, excludeWords []string, delayedDomains []string, currentState FeedState) (*FeedState, error) {

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

	notifyItems := getNotificationItems(feed, feedCfg, currentState.LastLink, excludeWords, delayedDomains, url)

	if currentState.LastLink == "" && len(notifyItems) > initialNotificationLimit {
		slog.Info("limiting notifications for new feed", "url", url, "count", len(notifyItems), "limit", initialNotificationLimit)
		notifyItems = notifyItems[:initialNotificationLimit]
	}

	if err := sendNotificationItems(ctx, notifyItems); err != nil {
		return &nextState, err
	}
	updateStateWithLatestItem(&nextState, currentState, notifyItems, feed, url, delayedDomains)

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

func getNotificationItems(feed *gofeed.Feed, feedCfg FeedFilterConfig, lastLink string, exclude []string, delayedDomains []string, feedURL string) []NotificationItem {
	var filteredCandidates []*gofeed.Item
	for _, item := range feed.Items {
		if passesFilters(item, feedCfg.IncludeKeywords, exclude) {
			filteredCandidates = append(filteredCandidates, item)
		}
	}

	startIndex := 0
	isDelayed := isDelayedDomain(feedURL, delayedDomains)
	if isDelayed {
		startIndex = delayedStartIndex
	}

	var notifyItems []NotificationItem
	for i := startIndex; i < len(filteredCandidates); i++ {
		item := filteredCandidates[i]

		if item.Link == lastLink {
			break
		}

		nItem := NotificationItem{
			Title:          item.Title,
			Link:           item.Link,
			Description:    cleanHTML(item.Description),
			FeedTitle:      feed.Title,
			SlackChannelID: feedCfg.SlackChannelID,
			EnableMastodon: feedCfg.EnableMastodon,
		}

		if isDelayed {
			if i+1 < len(filteredCandidates) {
				prevEntry := filteredCandidates[i+1]
				nItem.PreviousTitle = prevEntry.Title
				nItem.PreviousLink = prevEntry.Link
			}
		}

		notifyItems = append(notifyItems, nItem)
	}
	return notifyItems
}

func sendNotificationItems(ctx context.Context, items []NotificationItem) error {
	var errs []string
	for _, item := range items {
		if err := sendNotifications(ctx, item); err != nil {
			errs = append(errs, fmt.Sprintf("Notification failed for %s: %v", item.Title, err))
		}
		time.Sleep(notificationDelay)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, ", "))
	}
	return nil
}

func updateStateWithLatestItem(nextState *FeedState, currentState FeedState, notifyItems []NotificationItem, feed *gofeed.Feed, url string, delayedDomains []string) {
	if len(notifyItems) > 0 {
		mostRecent := notifyItems[0]
		nextState.LastLink = mostRecent.Link
	} else if currentState.LastLink == "" && len(feed.Items) > 0 {
		idx := 0
		if isDelayedDomain(url, delayedDomains) {
			idx = delayedStartIndex
		}
		if idx < len(feed.Items) {
			nextState.LastLink = feed.Items[idx].Link
		}
	}
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

func cleanHTML(htmlContent string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return htmlContent
	}
	text := strings.TrimSpace(doc.Text())

	return urlRegex.ReplaceAllString(text, "")
}

func sendNotifications(ctx context.Context, item NotificationItem) error {
	var errs []string

	logError := func(platform string, err error) {
		slog.Error("Notification failed",
			"error_type", "notification_error",
			"platform", platform,
			"item_title", item.Title,
			"url", item.Link,
			"error", err,
		)
	}

	if item.SlackChannelID != "" {
		if err := postToSlack(item); err != nil {
			logError("slack", err)
			errs = append(errs, fmt.Sprintf("Slack: %v", err))
		}
	}

	if item.EnableMastodon {
		if err := postToMastodon(ctx, item); err != nil {
			logError("mastodon", err)
			errs = append(errs, fmt.Sprintf("Mastodon: %v", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, ", "))
	}
	return nil
}

func postToSlack(item NotificationItem) error {
	api := slack.New(slackBotToken)

	_, _, err := api.PostMessage(
		item.SlackChannelID,
		slack.MsgOptionText(item.Format(true), false),
	)
	return err
}

func postToMastodon(ctx context.Context, item NotificationItem) error {
	c := mastodon.NewClient(&mastodon.Config{
		Server:      mastodonServer,
		AccessToken: mastodonAccessToken,
	})

	status := item.Format(false)
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
