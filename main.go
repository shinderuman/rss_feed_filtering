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
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/joho/godotenv"
	"github.com/mattn/go-mastodon"
	"github.com/mmcdole/gofeed"
	"github.com/slack-go/slack"
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
	rssHistoryMultiplier     = 2.0
	translationRetryCount    = 5
	gracefulShutdownBuffer   = 2 * time.Second
	translationConcurrency   = 3

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

	translationPromptTemplate = `あなたはRSSフィードの翻訳アシスタントです。
以下の <input> タグ内のテキストを日本語に翻訳・要約してください。
結果は必ず <json_schema> で定義されたJSONフォーマットのみで返してください。
Markdownコードブロック（` + "```json" + `など）や、JSON以外のテキスト（挨拶や説明など）は一切含めないでください。

<json_schema>
{
  "title": "翻訳されたタイトル（入力が空の場合は空文字）",
  "description": "翻訳・要約された説明文（入力が空の場合は空文字）"
}
</json_schema>

<input>
Title: %s
Description: %s
</input>

入力のTitleまたはDescriptionが空（あるいは意味を持たない文字列）の場合、出力JSONの対応するフィールドは必ず空文字 ("") にしてください。決して内容を捏造しないでください。`
)

var (
	s3Bucket                     string
	s3ConfigKey                  string
	s3StateKey                   string
	slackBotToken                string
	mastodonServer               string
	mastodonAccessToken          string
	anthropicAuthToken           string
	anthropicBaseURL             string
	anthropicDefaultModel        string
	urlRegex                     = regexp.MustCompile(`https?://[^\s]+`)
	translationTimeout           = 10 * time.Second
	translationInitialRetryDelay = 3 * time.Second
	translationRateLimitDelay    = 1100 * time.Millisecond

	translationRateLimiter = &RateLimiter{}
	translationSemaphore   = make(chan struct{}, translationConcurrency)
)

type RateLimiter struct {
	mu              sync.Mutex
	lastRequestTime time.Time
}

func (r *RateLimiter) Wait(ctx context.Context, delay time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	timeSinceLastRequest := time.Since(r.lastRequestTime)
	if timeSinceLastRequest < delay {
		sleepDuration := delay - timeSinceLastRequest
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleepDuration):
		}
	}
	r.lastRequestTime = time.Now()
}

type Config struct {
	GlobalExcludeWords []string           `json:"global_exclude_keywords"`
	DelayedDomains     []string           `json:"delayed_domains"`
	Configs            []FeedFilterConfig `json:"configs"`
	EnableSlack        bool               `json:"enable_slack"`
	EnableMastodon     bool               `json:"enable_mastodon"`
}

type FeedFilterConfig struct {
	IncludeKeywords     []string `json:"include_keywords"`
	ExcludeKeywords     []string `json:"exclude_keywords"`
	URLs                []string `json:"urls"`
	SlackChannelID      string   `json:"slack_channel_id"`
	EnableMastodon      bool     `json:"enable_mastodon"`
	EnableTranslation   bool     `json:"enable_translation"`
	EnableCollection    bool     `json:"enable_collection"`
	MastodonAccessToken string   `json:"mastodon_access_token"`
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
	Title               string
	Link                string
	Description         string
	FeedTitle           string
	SlackChannelID      string
	EnableSlack         bool
	EnableMastodon      bool
	MastodonAccessToken string
	PreviousTitle       string
	PreviousLink        string
	EnableCollection    bool
}

type FeedResult struct {
	Key           string
	State         FeedState
	Notifications []NotificationItem
}

type SaveStateFunc func(*State) error

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
	return executeWithRetry(ctx, func() (string, error) {
		message, err := t.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(t.model),
			MaxTokens: int64(anthropicMaxTokens),
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
			},
			Temperature: anthropic.Float(0.0),
		})

		if err != nil {
			return "", err
		}

		if len(message.Content) > 0 {
			return message.Content[0].Text, nil
		}
		return "", fmt.Errorf("no content in response")
	})
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

	notifications, processErr := processFeeds(ctx, appConfig, state, func(s *State) error {
		return saveState(ctx, s3Client, s)
	})
	if processErr != nil {
		slog.Error("Some feeds failed", "error", processErr)
	}

	sendAggregatedNotifications(ctx, notifications)

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

func processFeeds(ctx context.Context, appConfig *Config, oldState *State, saveStateFunc SaveStateFunc) ([][]NotificationItem, error) {
	newState := &State{Feeds: make(map[string]FeedState)}
	maps.Copy(newState.Feeds, oldState.Feeds)

	resultCh := make(chan FeedResult, len(appConfig.Configs)*5)

	var wg sync.WaitGroup

	for _, feedCfg := range appConfig.Configs {
		wg.Add(1)
		go processFeedConfig(ctx, feedCfg, appConfig, oldState, resultCh, &wg)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	batches, err := collectResults(ctx, resultCh, newState, saveStateFunc)
	if err != nil {
		return nil, err
	}

	if err := saveStateFunc(newState); err != nil {
		slog.Error("Failed to save state", "error", err)
		return nil, err
	}

	return batches, nil
}

func processFeedConfig(ctx context.Context, cfg FeedFilterConfig, appConfig *Config, oldState *State, out chan<- FeedResult, wg *sync.WaitGroup) {
	defer wg.Done()

	excludeWords := append(cfg.ExcludeKeywords, appConfig.GlobalExcludeWords...)
	sort.Strings(cfg.URLs)

	domainGroups := make(map[string][]string)
	for _, url := range cfg.URLs {
		h := getHostname(url)
		domainGroups[h] = append(domainGroups[h], url)
	}

	var innerWg sync.WaitGroup
	for _, urls := range domainGroups {
		innerWg.Add(1)
		go processDomainUrls(ctx, urls, cfg, appConfig, oldState, excludeWords, out, &innerWg)
	}
	innerWg.Wait()
}

func collectResults(ctx context.Context, resultCh <-chan FeedResult, newState *State, saveStateFunc SaveStateFunc) ([][]NotificationItem, error) {
	var batches [][]NotificationItem
	groupedItems := make(map[string][]NotificationItem)

	var shutdownCh <-chan time.Time
	if deadline, ok := ctx.Deadline(); ok {
		shutdownTimer := time.NewTimer(time.Until(deadline) - gracefulShutdownBuffer)
		shutdownCh = shutdownTimer.C
		defer shutdownTimer.Stop()
	}

	for {
		select {
		case res, ok := <-resultCh:
			if !ok {
				return finalizeBatches(batches, groupedItems), nil
			}
			newBatches := processFeedResult(res, newState, groupedItems)
			batches = append(batches, newBatches...)

		case <-shutdownCh:
			slog.Warn("Approaching Lambda timeout, saving state and performing graceful shutdown")
			if err := saveStateFunc(newState); err != nil {
				slog.Error("Failed to save state during graceful shutdown", "error", err)
				return nil, err
			}
			return nil, fmt.Errorf("graceful shutdown due to timeout")

		case <-ctx.Done():
			slog.Warn("Context done, saving state", "error", ctx.Err())
			if err := saveStateFunc(newState); err != nil {
				slog.Error("Failed to save state on context done", "error", err)
			}
			return nil, ctx.Err()
		}
	}
}

func processFeedResult(res FeedResult, newState *State, groupedItems map[string][]NotificationItem) [][]NotificationItem {
	newState.Feeds[res.Key] = res.State
	var newBatches [][]NotificationItem

	for _, item := range res.Notifications {
		if item.EnableCollection {
			groupedItems[item.SlackChannelID] = append(groupedItems[item.SlackChannelID], item)
		} else {
			newBatches = append(newBatches, []NotificationItem{item})
		}
	}
	return newBatches
}

func finalizeBatches(batches [][]NotificationItem, groupedItems map[string][]NotificationItem) [][]NotificationItem {
	for _, items := range groupedItems {
		batches = append(batches, items)
	}
	return batches
}

func processDomainUrls(ctx context.Context, urls []string, cfg FeedFilterConfig, appConfig *Config, oldState *State, excludeWords []string, out chan<- FeedResult, wg *sync.WaitGroup) {
	defer wg.Done()

	for i, url := range urls {
		if i > 0 {
			time.Sleep(domainDelay)
		}

		feedKey := fmt.Sprintf("%x", md5.Sum([]byte(url)))
		currentState := oldState.Feeds[feedKey]

		updatedFeedState, notifyItems, statusCode, err := processSingleFeed(ctx, url, cfg, excludeWords, appConfig, currentState)

		if err != nil {
			errorType := "fetch_error"
			if statusCode == http.StatusTooManyRequests {
				errorType = "rate_limit_error"
			}
			slog.Error("Failed to process feed", "error_type", errorType, "url", url, "error", err)
			continue
		}

		out <- FeedResult{
			Key:           feedKey,
			State:         *updatedFeedState,
			Notifications: notifyItems,
		}
	}
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

func processSingleFeed(ctx context.Context, url string, feedCfg FeedFilterConfig, excludeWords []string, appConfig *Config, currentState FeedState) (*FeedState, []NotificationItem, int, error) {

	feed, respHeaders, statusCode, err := fetchAndParse(ctx, url, currentState.LastModified, currentState.ETag)
	if err != nil {
		return nil, nil, statusCode, err
	}

	if statusCode == http.StatusNotModified {
		return &currentState, nil, statusCode, nil
	}

	nextState := currentState
	nextState.LastModified = respHeaders.Get("Last-Modified")
	nextState.ETag = respHeaders.Get("ETag")

	// Migration: If SeenLinks is empty but LastLink exists, initialize it
	if len(currentState.SeenLinks) == 0 && currentState.LastLink != "" {
		currentState.SeenLinks = []string{currentState.LastLink}
	}

	notifyItems := getNotificationItems(feed, feedCfg, currentState.SeenLinks, excludeWords, appConfig, url)

	if currentState.LastLink == "" && len(notifyItems) > initialNotificationLimit {
		slog.Info("limiting notifications for new feed", "url", url, "count", len(notifyItems), "limit", initialNotificationLimit)
		notifyItems = notifyItems[:initialNotificationLimit]
	}

	if feedCfg.EnableTranslation {
		client := anthropic.NewClient(
			option.WithAPIKey(anthropicAuthToken),
			option.WithBaseURL(anthropicBaseURL),
			option.WithHTTPClient(&http.Client{Timeout: translationTimeout}),
			option.WithMaxRetries(0),
		)
		translator := &AnthropicTranslator{
			client: &client,
			model:  anthropicDefaultModel,
		}
		notifyItems = translateNotificationItems(ctx, translator, notifyItems)
	}

	updateStateWithLatestItem(&nextState, currentState, notifyItems, feed, url, appConfig.DelayedDomains, len(feed.Items))

	return &nextState, notifyItems, statusCode, nil
}

func fetchAndParse(ctx context.Context, url string, lastModified, etag string) (*gofeed.Feed, http.Header, int, error) {
	headers := make(map[string]string)
	if lastModified != "" {
		headers["If-Modified-Since"] = lastModified
	}
	if etag != "" {
		headers["If-None-Match"] = etag
	}

	content, respHeaders, statusCode, err := fetchFeedContent(ctx, url, headers)
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

func fetchFeedContent(ctx context.Context, feedURL string, headers map[string]string) ([]byte, http.Header, int, error) {
	client := &http.Client{Timeout: defaultTimeout}
	req, err := http.NewRequestWithContext(ctx, "GET", feedURL, nil)
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

func getNotificationItems(feed *gofeed.Feed, feedCfg FeedFilterConfig, seenLinks []string, exclude []string, appConfig *Config, feedURL string) []NotificationItem {
	var filteredCandidates []*gofeed.Item
	for _, item := range feed.Items {
		if passesFilters(item, feedCfg.IncludeKeywords, exclude) {
			filteredCandidates = append(filteredCandidates, item)
		}
	}

	startIndex := 0
	isDelayed := isDelayedDomain(feedURL, appConfig.DelayedDomains)
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

		cleanDesc := cleanHTML(item.Description)
		if item.Title == "" && cleanDesc == "" {
			continue
		}

		nItem := NotificationItem{
			Title:               item.Title,
			Link:                item.Link,
			Description:         cleanDesc,
			FeedTitle:           feed.Title,
			SlackChannelID:      feedCfg.SlackChannelID,
			EnableSlack:         appConfig.EnableSlack,
			EnableMastodon:      appConfig.EnableMastodon && feedCfg.EnableMastodon,
			MastodonAccessToken: feedCfg.MastodonAccessToken,
			EnableCollection:    feedCfg.EnableCollection,
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

func sendAggregatedNotifications(ctx context.Context, batches [][]NotificationItem) {
	if len(batches) == 0 {
		return
	}

	var errs []string

	for _, batch := range batches {
		// Mastodon
		for _, item := range batch {
			if err := postToMastodon(ctx, item); err != nil {
				slog.Error("Notification failed",
					"error_type", "notification_error",
					"platform", "mastodon",
					"item_title", item.Title,
					"url", item.Link,
					"error", err,
				)
				errs = append(errs, fmt.Sprintf("Mastodon: %v", err))
			}
		}

		// Slack
		if err := postToSlack(batch); err != nil {
			slog.Error("Notification failed",
				"error_type", "notification_error",
				"platform", "slack",
				"count", len(batch),
				"error", err,
			)
			errs = append(errs, fmt.Sprintf("Slack(%s): %v", batch[0].SlackChannelID, err))
		}
	}

	if len(errs) > 0 {
		slog.Error("Some notifications failed", "errors", strings.Join(errs, ", "))
	}
}

func updateStateWithLatestItem(nextState *FeedState, currentState FeedState, notifyItems []NotificationItem, feed *gofeed.Feed, url string, delayedDomains []string, currentFeedCount int) {
	nextState.LastLink = determineLastLink(currentState, notifyItems, feed, url, delayedDomains)
	nextState.SeenLinks = updateSeenLinks(currentState.SeenLinks, notifyItems, currentFeedCount)
}

func determineLastLink(currentState FeedState, notifyItems []NotificationItem, feed *gofeed.Feed, url string, delayedDomains []string) string {
	if len(notifyItems) > 0 {
		return notifyItems[0].Link
	}

	if currentState.LastLink == "" && len(feed.Items) > 0 {
		idx := 0
		if isDelayedDomain(url, delayedDomains) {
			idx = delayedStartIndex
		}
		if idx < len(feed.Items) {
			return feed.Items[idx].Link
		}
	}

	return currentState.LastLink
}

func updateSeenLinks(currentSeenLinks []string, notifyItems []NotificationItem, currentFeedCount int) []string {
	var newLinks []string
	for _, item := range notifyItems {
		newLinks = append(newLinks, item.Link)
	}

	combined := append(newLinks, currentSeenLinks...)

	if currentFeedCount > 0 {
		limit := max(int(float64(currentFeedCount)*rssHistoryMultiplier), currentFeedCount)

		if len(combined) > limit {
			combined = combined[:limit]
		}
	}

	return combined
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

func postToSlack(items []NotificationItem) error {
	if len(items) == 0 {
		return nil
	}

	if !items[0].EnableSlack || items[0].SlackChannelID == "" {
		return nil
	}

	api := slack.New(slackBotToken)

	var sb strings.Builder
	for i, item := range items {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(item.Format(true))
	}

	_, _, err := api.PostMessage(
		items[0].SlackChannelID,
		slack.MsgOptionText(sb.String(), false),
	)
	return err
}

func postToMastodon(ctx context.Context, item NotificationItem) error {
	if !item.EnableMastodon {
		return nil
	}
	defer time.Sleep(notificationDelay)

	accessToken := mastodonAccessToken
	if item.MastodonAccessToken != "" {
		accessToken = item.MastodonAccessToken
	}
	c := mastodon.NewClient(&mastodon.Config{
		Server:      mastodonServer,
		AccessToken: accessToken,
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
	results := make([]NotificationItem, len(items))

	var wg sync.WaitGroup

	for i, item := range items {
		wg.Add(1)
		go func(i int, item NotificationItem) {
			defer wg.Done()

			translationSemaphore <- struct{}{}
			defer func() { <-translationSemaphore }()

			translationRateLimiter.Wait(ctx, translationRateLimitDelay)

			results[i] = translateSingleItem(ctx, translator, item)
		}(i, item)
	}

	wg.Wait()
	return results
}

func translateSingleItem(ctx context.Context, translator Translator, item NotificationItem) NotificationItem {
	prompt := fmt.Sprintf(translationPromptTemplate, item.Title, item.Description)

	resp, err := translator.Translate(ctx, prompt)
	if err != nil {
		slog.Error("Failed to translate item", "title", item.Title, "error", err)
		return item
	}

	type TranslationResponse struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}

	var translationResp TranslationResponse
	if err := json.Unmarshal([]byte(resp), &translationResp); err != nil {
		slog.Error("Failed to unmarshal translation response", "response", resp, "error", err)
		return item
	}

	if translationResp.Title != "" {
		item.Title = translationResp.Title
	}
	if translationResp.Description != "" {
		item.Description = translationResp.Description
	}

	return item
}

func executeWithRetry(ctx context.Context, op func() (string, error)) (string, error) {
	var lastErr error
	delay := translationInitialRetryDelay

	for i := 0; i <= translationRetryCount; i++ {
		res, err := op()
		if err == nil {
			return res, nil
		}

		lastErr = err

		if !shouldRetry(err) {
			return "", err
		}

		if i < translationRetryCount {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
				delay *= 2
			}
		}
	}

	return "", lastErr
}

func shouldRetry(err error) bool {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway:
			return true
		}
	}

	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "deadline exceeded")
}
