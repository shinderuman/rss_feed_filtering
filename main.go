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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/slack-go/slack"

	"github.com/PuerkitoBio/goquery"
	"github.com/joho/godotenv"
	"github.com/mattn/go-mastodon"
	"github.com/mmcdole/gofeed"
)

const (
	userAgent                = "RSS-Filter-Bot/1.0"
	defaultTimeout           = 10 * time.Second
	anthropicMaxTokens       = 1024
	mastodonMaxStatusLength  = 500
	delayedStartIndex        = 1
	domainDelay              = 500 * time.Millisecond
	notificationDelay        = 200 * time.Millisecond // Initial notification limit for new feeds
	initialNotificationLimit = 5
	maxSeenLinks             = 100

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
	s3Bucket              string
	s3ConfigKey           string
	s3StateKey            string
	slackBotToken         string
	mastodonServer        string
	mastodonAccessToken   string
	anthropicAuthToken    string
	anthropicBaseURL      string
	anthropicDefaultModel string
	urlRegex              = regexp.MustCompile(`https?://[^\s]+`)
)

type Config struct {
	GlobalExcludeWords []string           `json:"global_exclude_keywords"`
	DelayedDomains     []string           `json:"delayed_domains"`
	Configs            []FeedFilterConfig `json:"configs"`
}

type FeedFilterConfig struct {
	IncludeKeywords   []string `json:"include_keywords"`
	ExcludeKeywords   []string `json:"exclude_keywords"`
	URLs              []string `json:"urls"`
	SlackChannelID    string   `json:"slack_channel_id"`
	EnableMastodon    bool     `json:"enable_mastodon"`
	EnableTranslation bool     `json:"enable_translation"`
}

type State struct {
	Feeds map[string]FeedState `json:"feeds"`
}

type FeedState struct {
	LastLink     string   `json:"last_link"`
	LastModified string   `json:"last_modified"`
	ETag         string   `json:"etag"`
	SeenLinks    []string `json:"seen_links"`
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

type Translator interface {
	Translate(ctx context.Context, prompt string) (string, error)
}

type AnthropicTranslator struct {
	client *anthropic.Client
	model  string
}

func (t *AnthropicTranslator) Translate(ctx context.Context, prompt string) (string, error) {
	message, err := t.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(t.model),
		MaxTokens: int64(anthropicMaxTokens),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}

	if len(message.Content) > 0 {
		return message.Content[0].Text, nil
	}
	return "", fmt.Errorf("no content in response")
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
		_ = godotenv.Load()
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
	anthropicAuthToken = os.Getenv("ANTHROPIC_AUTH_TOKEN")
	anthropicBaseURL = os.Getenv("ANTHROPIC_BASE_URL")
	anthropicDefaultModel = os.Getenv("ANTHROPIC_DEFAULT_MODEL")
}

func loadAppConfig(ctx context.Context, client *s3.Client) (*Config, error) {
	resp, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(s3ConfigKey),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

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
	defer resp.Body.Close() //nolint:errcheck

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

			updatedFeedState, statusCode, err := processSingleFeed(ctx, url, feedCfg, excludeWords, appConfig.DelayedDomains, currentState)
			if err != nil {
				// Check if error is HTTP 429 (Too Many Requests)
				errorType := "fetch_error"
				if statusCode == http.StatusTooManyRequests {
					errorType = "rate_limit_error"
				}

				slog.Error("Failed to process feed",
					"error_type", errorType,
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

func processSingleFeed(ctx context.Context, url string, feedCfg FeedFilterConfig, excludeWords []string, delayedDomains []string, currentState FeedState) (*FeedState, int, error) {

	feed, respHeaders, statusCode, err := fetchAndParse(url, currentState.LastModified, currentState.ETag)
	if err != nil {
		return nil, statusCode, err
	}

	if statusCode == http.StatusNotModified {
		return &currentState, statusCode, nil
	}

	nextState := currentState
	nextState.LastModified = respHeaders.Get("Last-Modified")
	nextState.ETag = respHeaders.Get("ETag")

	// Migration: If SeenLinks is empty but LastLink exists, initialize it
	if len(currentState.SeenLinks) == 0 && currentState.LastLink != "" {
		currentState.SeenLinks = []string{currentState.LastLink}
	}

	notifyItems := getNotificationItems(feed, feedCfg, currentState.SeenLinks, excludeWords, delayedDomains, url)

	if feedCfg.EnableTranslation {
		client := anthropic.NewClient(
			option.WithAPIKey(anthropicAuthToken),
			option.WithBaseURL(anthropicBaseURL),
		)
		translator := &AnthropicTranslator{
			client: &client,
			model:  anthropicDefaultModel,
		}
		notifyItems = translateNotificationItems(ctx, translator, notifyItems)
	}

	if currentState.LastLink == "" && len(notifyItems) > initialNotificationLimit {
		slog.Info("limiting notifications for new feed", "url", url, "count", len(notifyItems), "limit", initialNotificationLimit)
		notifyItems = notifyItems[:initialNotificationLimit]
	}

	if err := sendNotificationItems(ctx, notifyItems); err != nil {
		return &nextState, statusCode, err
	}
	updateStateWithLatestItem(&nextState, currentState, notifyItems, feed, url, delayedDomains)

	return &nextState, statusCode, nil
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
		return nil, nil, statusCode, err
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
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header, resp.StatusCode, nil
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	return body, resp.Header, resp.StatusCode, err
}

func getNotificationItems(feed *gofeed.Feed, feedCfg FeedFilterConfig, seenLinks []string, exclude []string, delayedDomains []string, feedURL string) []NotificationItem {
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

		// If we encounter a link we've seen before, we assume we've reached the point of previous checks.
		// This is more robust than checking just the single LastLink.
		if slices.Contains(seenLinks, item.Link) {
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
	// Update LastLink for backward compatibility or reference
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

	// Update SeenLinks
	// 1. Gather new links from notifyItems (descending order in slice = Newest to Oldest)
	//    Wait, feed.Items are Newest-First. loop runs 0..N.
	//    notifyItems are appended in order of appearance (Newest -> Oldest).
	//    So notifyItems[0] is the Newest.
	var newLinks []string
	for _, item := range notifyItems {
		newLinks = append(newLinks, item.Link)
	}

	// 2. Prepend new links to existing SeenLinks (so index 0 is always newest)
	//    Handling potential duplicates if logic wasn't perfect?
	//    We can just prepend. The dedupe logic in reading prevents adding same things again mostly.
	combined := append(newLinks, currentState.SeenLinks...)

	// 3. Truncate to maxSeenLinks
	if len(combined) > maxSeenLinks {
		combined = combined[:maxSeenLinks]
	}
	nextState.SeenLinks = combined
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

func translateNotificationItems(ctx context.Context, translator Translator, items []NotificationItem) []NotificationItem {
	var translatedItems []NotificationItem
	for _, item := range items {
		// Translate Title
		titlePrompt := fmt.Sprintf("以下のRSSフィードのタイトルを日本語に翻訳してください。結果の文字列のみを返してください。余計な説明は不要です。\n\nTitle: %s", item.Title)
		if translatedTitle, err := translator.Translate(ctx, titlePrompt); err == nil {
			item.Title = translatedTitle
		} else {
			slog.Error("Failed to translate title", "title", item.Title, "error", err)
		}

		// Translate Description
		descPrompt := fmt.Sprintf("以下のRSSフィードの説明文を日本語に翻訳・要約してください。結果の文字列のみを返してください。余計な説明は不要です。\n\nDescription: %s", item.Description)
		if translatedDesc, err := translator.Translate(ctx, descPrompt); err == nil {
			item.Description = translatedDesc
		} else {
			slog.Error("Failed to translate description", "description", item.Description, "error", err)
		}

		translatedItems = append(translatedItems, item)
	}
	return translatedItems
}
