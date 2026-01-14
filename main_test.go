package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// MockTranslator for testing
type MockTranslator struct {
	TranslateFunc func(ctx context.Context, prompt string) (string, error)
}

func (m *MockTranslator) Translate(ctx context.Context, prompt string) (string, error) {
	if m.TranslateFunc != nil {
		return m.TranslateFunc(ctx, prompt)
	}
	return "Translated Text", nil
}

func TestUpdateStateWithLatestItem(t *testing.T) {
	current := FeedState{
		SeenLinks: []string{"old1", "old2"},
	}
	notifyItems := []NotificationItem{
		{Link: "new1"},
		{Link: "new2"},
	}
	feed := &gofeed.Feed{}

	nextState := &FeedState{}

	// Feed count 100 -> limit 200, no truncation
	updateStateWithLatestItem(nextState, current, notifyItems, feed, "", nil, 100)

	expected := []string{"new1", "new2", "old1", "old2"}
	if !reflect.DeepEqual(nextState.SeenLinks, expected) {
		t.Errorf("Expected %v, got %v", expected, nextState.SeenLinks)
	}

	if nextState.LastLink != "new1" {
		t.Errorf("Expected LastLink to be new1, got %s", nextState.LastLink)
	}
}

func TestGetNotificationItems_SeenBreaks(t *testing.T) {
	feed := &gofeed.Feed{
		Items: []*gofeed.Item{
			{Title: "Item 1", Link: "link1"},
			{Title: "Item 2", Link: "link2"},
			{Title: "Item 3", Link: "link3"}, // SEEN
			{Title: "Item 4", Link: "link4"},
		},
	}

	cfg := FeedFilterConfig{}
	seenLinks := []string{"link3", "other"}

	items := getNotificationItems(feed, cfg, seenLinks, nil, &Config{}, "")

	if len(items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(items))
	}
	if items[0].Link != "link1" {
		t.Errorf("Expected first item to be link1, got %s", items[0].Link)
	}
	if items[1].Link != "link2" {
		t.Errorf("Expected second item to be link2, got %s", items[1].Link)
	}

	// Ensure link4 was NOT included (because we broke at link3)
}

func TestGetNotificationItems_NoSeen(t *testing.T) {
	feed := &gofeed.Feed{
		Items: []*gofeed.Item{
			{Title: "Item 1", Link: "link1"},
			{Title: "Item 2", Link: "link2"},
		},
	}

	cfg := FeedFilterConfig{}
	seenLinks := []string{"link3"} // link3 is not in feed

	items := getNotificationItems(feed, cfg, seenLinks, nil, &Config{}, "")

	if len(items) != 2 {
		t.Errorf("Expected 2 items, got %d", len(items))
	}
}

func TestGetNotificationItems_EmptyFiltering(t *testing.T) {
	feed := &gofeed.Feed{
		Items: []*gofeed.Item{
			{Title: "Valid Item", Link: "link1", Description: "Desc"},
			{Title: "", Link: "link2", Description: ""},       // Should be filtered
			{Title: "", Link: "link3", Description: " <br> "}, // Should be filtered (empty after clean)
		},
	}

	cfg := FeedFilterConfig{}
	seenLinks := []string{}

	items := getNotificationItems(feed, cfg, seenLinks, nil, &Config{}, "")

	if len(items) != 1 {
		t.Errorf("Expected 1 item, got %d", len(items))
	}
	if items[0].Link != "link1" {
		t.Errorf("Expected only valid item (link1), got %s", items[0].Link)
	}
}

func TestTranslateNotificationItems(t *testing.T) {
	ctx := context.Background()
	items := []NotificationItem{
		{Title: "Original Title", Description: "Original Description"},
	}

	// Test Success Case
	mock := &MockTranslator{
		TranslateFunc: func(ctx context.Context, prompt string) (string, error) {
			if strings.Contains(prompt, "Title:") && strings.Contains(prompt, "Description:") {
				return `{"title": "翻訳されたタイトル", "description": "翻訳された説明"}`, nil
			}
			return "", fmt.Errorf("unknown prompt structure")
		},
	}

	translated := translateNotificationItems(ctx, mock, items)

	if len(translated) != 1 {
		t.Fatalf("Expected 1 item, got %d", len(translated))
	}
	if translated[0].Title != "翻訳されたタイトル" {
		t.Errorf("Expected title '翻訳されたタイトル', got '%s'", translated[0].Title)
	}
	if translated[0].Description != "翻訳された説明" {
		t.Errorf("Expected description '翻訳された説明', got '%s'", translated[0].Description)
	}

	// Test Error Case (API Error)
	errorMock := &MockTranslator{
		TranslateFunc: func(ctx context.Context, prompt string) (string, error) {
			return "", fmt.Errorf("API error")
		},
	}

	fallbackItems := translateNotificationItems(ctx, errorMock, items)
	if len(fallbackItems) != 1 {
		t.Fatalf("Expected 1 item, got %d", len(fallbackItems))
	}
	if fallbackItems[0].Title != "Original Title" {
		t.Errorf("Expected fallback title 'Original Title', got '%s'", fallbackItems[0].Title)
	}

	// Test Error Case (JSON Unmarshal Error)
	jsonErrorMock := &MockTranslator{
		TranslateFunc: func(ctx context.Context, prompt string) (string, error) {
			return `INVALID JSON`, nil
		},
	}
	jsonFallbackItems := translateNotificationItems(ctx, jsonErrorMock, items)
	if len(jsonFallbackItems) != 1 {
		t.Fatalf("Expected 1 item, got %d", len(jsonFallbackItems))
	}
	if jsonFallbackItems[0].Title != "Original Title" {
		t.Errorf("Expected fallback title 'Original Title' on JSON error, got '%s'", jsonFallbackItems[0].Title)
	}
}

func TestTranslateNotificationItems_Results(t *testing.T) {
	ctx := context.Background()
	items := []NotificationItem{
		{Title: "Item 1", Description: "Desc 1"},
		{Title: "Item 2", Description: "Desc 2"},
		{Title: "Item 3", Description: "Desc 3"},
		{Title: "Item 4", Description: "Desc 4"},
	}

	delay := 10 * time.Millisecond

	// Mock translator with delay
	mock := &MockTranslator{
		TranslateFunc: func(ctx context.Context, prompt string) (string, error) {
			time.Sleep(delay)
			// Return a JSON that includes the input title to verify order
			var title string
			if strings.Contains(prompt, "Item 1") {
				title = "Translated 1"
			} else if strings.Contains(prompt, "Item 2") {
				title = "Translated 2"
			} else if strings.Contains(prompt, "Item 3") {
				title = "Translated 3"
			} else if strings.Contains(prompt, "Item 4") {
				title = "Translated 4"
			}
			return fmt.Sprintf(`{"title": "%s", "description": "Translated Desc"}`, title), nil
		},
	}

	results := translateNotificationItems(ctx, mock, items)

	// Verification
	if len(results) != 4 {
		t.Fatalf("Expected 4 items, got %d", len(results))
	}

	// Verify Order
	for i := 0; i < 4; i++ {
		expectedTitle := fmt.Sprintf("Translated %d", i+1)
		if results[i].Title != expectedTitle {
			t.Errorf("Index %d: Expected title '%s', got '%s'", i, expectedTitle, results[i].Title)
		}
	}
}

func TestProcessFeeds(t *testing.T) {
	// Mock RSS Server with delay to verify parallelism
	serverDelay := 200 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		rss := `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0">
<channel>
 <title>Mock RSS</title>
 <item>
  <title>Test Item</title>
  <link>http://example.com/item</link>
 </item>
</channel>
</rss>`
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, rss)
	}))
	defer ts.Close()

	ctx := context.Background()
	// Create 2 configs to trigger 2 goroutines
	appConfig := &Config{
		Configs: []FeedFilterConfig{
			{URLs: []string{ts.URL}},
			{URLs: []string{ts.URL + "/2"}},
		},
	}
	oldState := &State{
		Feeds: map[string]FeedState{},
	}

	// Capture state
	var capturedState *State
	var mu sync.Mutex
	saveFunc := func(s *State) error {
		mu.Lock()
		defer mu.Unlock()
		capturedState = s
		return nil
	}

	start := time.Now()
	_, err := processFeeds(ctx, appConfig, oldState, saveFunc)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("processFeeds failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedState == nil {
		t.Fatal("saveStateFunc was never called")
	}

	if len(capturedState.Feeds) != 2 {
		t.Errorf("Expected 2 feed states, got %d", len(capturedState.Feeds))
	}

	// If sequential: (200ms server + 200ms notification) * 2 = 800ms
	// If parallel: 200ms server + 200ms notification = 400ms (plus overhead)
	// We assert it is significantly faster than sequential.
	if elapsed >= 600*time.Millisecond {
		t.Errorf("Execution took %v, expected less than 600ms (parallel execution failed)", elapsed)
	}
}

func TestProcessFeeds_InnerParallelism(t *testing.T) {
	// Mock RSS Server
	serverDelay := 200 * time.Millisecond
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		rss := `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0">
<channel>
 <title>Mock RSS</title>
 <item>
  <title>Test Item</title>
  <link>http://example.com/item</link>
 </item>
</channel>
</rss>`
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, rss)
	}))
	defer ts.Close()

	ctx := context.Background()

	// Create ONE config with 2 URLs having DIFFERENT HOSTNAMES
	url1 := ts.URL
	url2 := strings.Replace(ts.URL, "127.0.0.1", "localhost", 1)

	appConfig := &Config{
		Configs: []FeedFilterConfig{
			{URLs: []string{url1, url2}},
		},
	}
	oldState := &State{
		Feeds: make(map[string]FeedState),
	}

	// Capture state
	var capturedState *State
	var mu sync.Mutex
	saveFunc := func(s *State) error {
		mu.Lock()
		defer mu.Unlock()
		capturedState = s
		return nil
	}

	start := time.Now()
	notifications, err := processFeeds(ctx, appConfig, oldState, saveFunc)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("processFeeds failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedState == nil {
		t.Fatal("saveStateFunc was never called")
	}

	if len(capturedState.Feeds) != 2 {
		t.Errorf("Expected 2 feed states, got %d", len(capturedState.Feeds))
	}

	// Verify time
	// If grouped (same domain): sequential -> 800ms
	// If separated (diff domain): parallel -> 400ms
	if elapsed >= 600*time.Millisecond {
		t.Errorf("Execution took %v, expected less than 600ms (inner parallel execution failed)", elapsed)
	}

	// Verification of aggregation
	totalNotifications := 0
	for _, batch := range notifications {
		totalNotifications += len(batch)
	}
	if totalNotifications != 2 {
		t.Errorf("Expected 2 total notifications, got %d", totalNotifications)
	}
}

func TestProcessFeeds_ParallelStateUpdate(t *testing.T) {
	// 1. Setup Mock Servers
	// Server 1: Returns a NEW item (Success)
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rss := `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0">
<channel>
 <title>Mock RSS 1</title>
 <item>
  <title>New Item 1</title>
  <link>http://example.com/new1</link>
 </item>
</channel>
</rss>`
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, rss)
	}))
	defer ts1.Close()

	// Server 2: Returns 500 Error (Failure)
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts2.Close()

	// Server 3: Returns NEW item (Success)
	ts3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rss := `<?xml version="1.0" encoding="UTF-8" ?>
<rss version="2.0">
<channel>
 <title>Mock RSS 3</title>
 <item>
  <title>New Item 3</title>
  <link>http://example.com/new3</link>
 </item>
</channel>
</rss>`
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, rss)
	}))
	defer ts3.Close()

	// 2. Setup Config and Old State
	ctx := context.Background()
	appConfig := &Config{
		Configs: []FeedFilterConfig{
			{URLs: []string{ts1.URL, ts2.URL}}, // Group 1 (Mixed success/fail)
			{URLs: []string{ts3.URL}},          // Group 2 (Success)
		},
	}

	// Compute keys
	key1 := fmt.Sprintf("%x", md5.Sum([]byte(ts1.URL)))
	key2 := fmt.Sprintf("%x", md5.Sum([]byte(ts2.URL)))
	key3 := fmt.Sprintf("%x", md5.Sum([]byte(ts3.URL)))

	oldState := &State{
		Feeds: map[string]FeedState{
			key1: {SeenLinks: []string{"old1"}},
			key2: {SeenLinks: []string{"old2"}}, // Should be preserved
			key3: {SeenLinks: []string{"old3"}},
		},
	}

	// 3. Run
	var capturedState *State
	var mu sync.Mutex
	saveFunc := func(s *State) error {
		mu.Lock()
		defer mu.Unlock()
		capturedState = s
		return nil
	}

	_, err := processFeeds(ctx, appConfig, oldState, saveFunc)
	if err != nil {
		t.Fatalf("processFeeds failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if capturedState == nil {
		t.Fatal("saveStateFunc was never called")
	}

	// 4. Verify
	newState := capturedState

	// Check TS1 (Updated)
	if state, ok := newState.Feeds[key1]; !ok {
		t.Errorf("Key1 missing from state")
	} else {
		if state.LastLink != "http://example.com/new1" {
			t.Errorf("Key1 LastLink not updated. Got: %s", state.LastLink)
		}
		if len(state.SeenLinks) < 2 || state.SeenLinks[0] != "http://example.com/new1" {
			t.Errorf("Key1 SeenLinks not updated correctly. Got: %v", state.SeenLinks)
		}
	}

	// Check TS2 (Failed, should be preserved)
	if state, ok := newState.Feeds[key2]; !ok {
		t.Errorf("Key2 missing from state")
	} else {
		// Should match old state exactly?
		// Or effectively match.
		if len(state.SeenLinks) != 1 || state.SeenLinks[0] != "old2" {
			t.Errorf("Key2 data corrupted or changed. Got: %v", state.SeenLinks)
		}
	}

	// Check TS3 (Updated)
	if state, ok := newState.Feeds[key3]; !ok {
		t.Errorf("Key3 missing from state")
	} else {
		if state.LastLink != "http://example.com/new3" {
			t.Errorf("Key3 LastLink not updated. Got: %s", state.LastLink)
		}
		if len(state.SeenLinks) < 2 || state.SeenLinks[0] != "http://example.com/new3" {
			t.Errorf("Key3 SeenLinks not updated correctly. Got: %v", state.SeenLinks)
		}
	}
}

func TestProcessFeeds_SaveOnce(t *testing.T) {
	// Setup: 2 successful feeds
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<rss version="2.0"><channel><item><title>Item1</title><link>http://example.com/1</link></item></channel></rss>`)
	}))
	defer ts1.Close()

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<rss version="2.0"><channel><item><title>Item2</title><link>http://example.com/2</link></item></channel></rss>`)
	}))
	defer ts2.Close()

	ctx := context.Background()
	appConfig := &Config{
		Configs: []FeedFilterConfig{
			{URLs: []string{ts1.URL}},
			{URLs: []string{ts2.URL}},
		},
	}

	oldState := &State{Feeds: make(map[string]FeedState)}

	// Capture save calls
	var saveCallCount int
	var mu sync.Mutex

	saveFunc := func(s *State) error {
		mu.Lock()
		defer mu.Unlock()
		saveCallCount++
		// Verify that the state passed here is valid and not empty
		if len(s.Feeds) == 0 {
			return fmt.Errorf("saved empty state")
		}
		return nil
	}

	_, err := processFeeds(ctx, appConfig, oldState, saveFunc)
	if err != nil {
		t.Fatalf("processFeeds failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// saveFunc should be called exactly once
	if saveCallCount != 1 {
		t.Errorf("Expected exactly 1 save call, got %d", saveCallCount)
	}
}

func TestProcessFeeds_Idempotency(t *testing.T) {
	// Setup: Feed with 1 item
	rssContent := `<rss version="2.0"><channel><item><title>Item1</title><link>http://example.com/1</link></item></channel></rss>`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, rssContent)
	}))
	defer ts.Close()

	ctx := context.Background()
	appConfig := &Config{Configs: []FeedFilterConfig{{URLs: []string{ts.URL}}}}

	// Run 1: Initial State (Empty)
	state1 := &State{Feeds: make(map[string]FeedState)}
	var savedState *State
	saveFunc := func(s *State) error {
		savedState = s
		return nil
	}

	if _, err := processFeeds(ctx, appConfig, state1, saveFunc); err != nil {
		t.Fatalf("Run 1 failed: %v", err)
	}

	// Verify Run 1 State
	key := fmt.Sprintf("%x", md5.Sum([]byte(ts.URL)))
	if len(savedState.Feeds[key].SeenLinks) != 1 {
		t.Fatalf("Run 1: Expected 1 seen link, got %d", len(savedState.Feeds[key].SeenLinks))
	}

	state2 := savedState
	var savedState2 *State
	saveFunc2 := func(s *State) error {
		savedState2 = s
		return nil
	}

	notifications2, err := processFeeds(ctx, appConfig, state2, saveFunc2)
	if err != nil {
		t.Fatalf("Run 2 failed: %v", err)
	}

	if len(notifications2) > 0 {
		t.Errorf("Run 2: Expected 0 notifications, got %d groups", len(notifications2))
	}

	if len(savedState2.Feeds[key].SeenLinks) != 1 {
		t.Errorf("Run 2: Expected SeenLinks length to remain 1, got %d", len(savedState2.Feeds[key].SeenLinks))
	}
	if savedState2.Feeds[key].SeenLinks[0] != "http://example.com/1" {
		t.Errorf("Run 2: SeenLinks corrupted")
	}
}

func TestGetNotificationItems_WithMastodonToken(t *testing.T) {
	feed := &gofeed.Feed{
		Items: []*gofeed.Item{
			{Title: "Item 1", Link: "link1"},
		},
	}

	cfg := FeedFilterConfig{
		EnableMastodon:      true,
		MastodonAccessToken: "custom_token",
	}
	seenLinks := []string{}

	items := getNotificationItems(feed, cfg, seenLinks, nil, &Config{EnableMastodon: true}, "")

	if len(items) != 1 {
		t.Errorf("Expected 1 item, got %d", len(items))
	}
	if items[0].MastodonAccessToken != "custom_token" {
		t.Errorf("Expected token 'custom_token', got '%s'", items[0].MastodonAccessToken)
	}
}

func TestGlobalFlags_Control(t *testing.T) {
	feed := &gofeed.Feed{
		Items: []*gofeed.Item{
			{Title: "Item 1", Link: "link1"},
		},
	}
	cfg := FeedFilterConfig{
		EnableMastodon: true,
		SlackChannelID: "C123",
	}
	seenLinks := []string{}

	// Case 1: Global Flags False (Default)
	appConfig := &Config{
		EnableSlack:    false,
		EnableMastodon: false,
	}
	items := getNotificationItems(feed, cfg, seenLinks, nil, appConfig, "")

	if len(items) != 1 {
		t.Errorf("Expected 1 item")
	}
	if items[0].EnableSlack {
		t.Errorf("Expected EnableSlack to be false")
	}
	if items[0].EnableMastodon {
		t.Errorf("Expected EnableMastodon to be false")
	}

	// Case 2: Global Flags True
	appConfig.EnableSlack = true
	appConfig.EnableMastodon = true
	items = getNotificationItems(feed, cfg, seenLinks, nil, appConfig, "")

	if !items[0].EnableSlack {
		t.Errorf("Expected EnableSlack to be true")
	}
	if !items[0].EnableMastodon {
		t.Errorf("Expected EnableMastodon to be true")
	}
}

func TestAnthropicTranslator_Translate_Timeout_Retry(t *testing.T) {
	setupFastTest(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			// Simulate timeout
			time.Sleep(translationTimeout + 2*time.Second)
			// Even if we write response, client should have timed out
			w.WriteHeader(http.StatusOK)
			return
		}

		// Success on 3rd attempt
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"message","id":"msg_123","role":"assistant","content":[{"type":"text","text":"Translated"}]}`))
	}))
	defer ts.Close()

	// 2. Client configuration
	client := anthropic.NewClient(
		option.WithAPIKey("dummy"),
		option.WithBaseURL(ts.URL),
		option.WithHTTPClient(&http.Client{Timeout: translationTimeout}),
		option.WithMaxRetries(0),
	)

	translator := &AnthropicTranslator{
		client: &client,
		model:  "claude-dummy",
	}

	// 3. Execution
	start := time.Now()
	res, err := translator.Translate(context.Background(), "test")
	duration := time.Since(start)

	// 4. Verification
	if err != nil {
		t.Fatalf("Expected success after retries, got error: %v", err)
	}

	if res != "Translated" {
		t.Errorf("Expected 'Translated', got '%s'", res)
	}

	expectedMin := 1 * time.Millisecond
	if duration < expectedMin {
		t.Errorf("Expected duration >= %v, got %v", expectedMin, duration)
	}

	t.Logf("Retry success confirmed. Duration: %v, Attempts: %d", duration, attempts)
}

func TestAnthropicTranslator_Translate_Retry429(t *testing.T) {
	setupFastTest(t)

	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			// Anthropic error format
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"message","id":"msg_123","role":"assistant","content":[{"type":"text","text":"Success"}]}`))
	}))
	defer ts.Close()

	// 2. Client setup
	client := anthropic.NewClient(
		option.WithAPIKey("dummy"),
		option.WithBaseURL(ts.URL),
		option.WithHTTPClient(&http.Client{Timeout: 1 * time.Second}),
		option.WithMaxRetries(0), // Ensure we rely on our own retry logic
	)

	translator := &AnthropicTranslator{
		client: &client,
		model:  "claude-test",
	}

	// 3. Execution
	start := time.Now()
	res, err := translator.Translate(context.Background(), "test")
	duration := time.Since(start)

	// 4. Verification
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}
	if res != "Success" {
		t.Errorf("Expected 'Success', got '%s'", res)
	}

	expectedMin := 1 * time.Millisecond
	if duration < expectedMin {
		t.Errorf("Expected duration >= %v, got %v", expectedMin, duration)
	}
}

func TestAnthropicTranslator_Translate_Retry502(t *testing.T) {
	setupFastTest(t)

	// 1. Mock server that returns 502 twice, then 200
	var attempts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			// Anthropic error format for 502
			w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"Bad Gateway"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"message","id":"msg_123","role":"assistant","content":[{"type":"text","text":"Success"}]}`))
	}))
	defer ts.Close()

	// 2. Client setup
	client := anthropic.NewClient(
		option.WithAPIKey("dummy"),
		option.WithBaseURL(ts.URL),
		option.WithHTTPClient(&http.Client{Timeout: 1 * time.Second}),
		option.WithMaxRetries(0), // Ensure we rely on our own retry logic
	)

	translator := &AnthropicTranslator{
		client: &client,
		model:  "claude-test",
	}

	// 3. Execution
	start := time.Now()
	res, err := translator.Translate(context.Background(), "test")
	duration := time.Since(start)

	// 4. Verification
	if err != nil {
		t.Fatalf("Expected success, got error: %v", err)
	}
	if res != "Success" {
		t.Errorf("Expected 'Success', got '%s'", res)
	}

	expectedMin := 1 * time.Millisecond
	if duration < expectedMin {
		t.Errorf("Expected duration >= %v, got %v", expectedMin, duration)
	}
}

func TestCollectResults_CollectionLogic(t *testing.T) {
	// Setup resultCh with mixed items
	resultCh := make(chan FeedResult, 3)

	// Group 1: Collection Enabled (Channel A) - simulating 2 items from one feed
	resultCh <- FeedResult{
		Notifications: []NotificationItem{
			{Title: "A1", SlackChannelID: "ChannelA", EnableCollection: true},
			{Title: "A2", SlackChannelID: "ChannelA", EnableCollection: true},
		},
	}

	// Group 2: Collection Disabled (Channel A) - separate feed or item
	resultCh <- FeedResult{
		Notifications: []NotificationItem{
			{Title: "A3", SlackChannelID: "ChannelA", EnableCollection: false},
		},
	}

	// Group 3: Collection Enabled (Channel B)
	resultCh <- FeedResult{
		Notifications: []NotificationItem{
			{Title: "B1", SlackChannelID: "ChannelB", EnableCollection: true},
		},
	}

	close(resultCh)

	newState := &State{Feeds: make(map[string]FeedState)}
	saveFunc := func(s *State) error { return nil }

	batches, _ := collectResults(context.Background(), resultCh, newState, saveFunc)

	// Expectations:
	// batch for A1, A2 (grouped)
	// batch for A3 (individual)
	// batch for B1 (grouped)
	// Total 3 batches

	if len(batches) != 3 {
		t.Errorf("Expected 3 batches, got %d", len(batches))
	}

	foundA3 := false
	foundA1A2 := false
	foundB1 := false

	for _, b := range batches {
		if len(b) > 0 && b[0].SlackChannelID == "ChannelA" {
			if len(b) == 1 && b[0].Title == "A3" {
				foundA3 = true
			} else if len(b) == 2 {
				// Order within group should be preserved as inserted
				if b[0].Title == "A1" && b[1].Title == "A2" {
					foundA1A2 = true
				}
			}
		} else if len(b) > 0 && b[0].SlackChannelID == "ChannelB" {
			if len(b) == 1 && b[0].Title == "B1" {
				foundB1 = true
			}
		}
	}

	if !foundA3 {
		t.Error("Did not find A3 individual batch")
	}
	if !foundA1A2 {
		t.Error("Did not find A1+A2 grouped batch")
	}
	if !foundB1 {
		t.Error("Did not find B1 grouped batch")
	}
}

func TestProcessFeeds_TimeoutSave(t *testing.T) {
	// Setup: Mock server that simulates a slow feed
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // Simulating work longer than timeout buffer interaction
		fmt.Fprint(w, `<rss version="2.0"><channel><item><title>Item1</title><link>http://example.com/1</link></item></channel></rss>`)
	}))
	defer ts.Close()

	appConfig := &Config{
		Configs: []FeedFilterConfig{
			{URLs: []string{ts.URL}},
		},
	}
	oldState := &State{Feeds: make(map[string]FeedState)}

	// Verify save is called
	var saveCallCount int
	var mu sync.Mutex
	saveFunc := func(s *State) error {
		mu.Lock()
		defer mu.Unlock()
		saveCallCount++
		return nil
	}

	// Create a context with a deadline that triggers graceful shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownBuffer+100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := processFeeds(ctx, appConfig, oldState, saveFunc)
	elapsed := time.Since(start)

	// We expect an error "graceful shutdown due to timeout"
	if err == nil {
		t.Fatal("Expected timeout error, got nil")
	}
	if err.Error() != "graceful shutdown due to timeout" {
		t.Errorf("Expected 'graceful shutdown due to timeout', got '%v'", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if saveCallCount == 0 {
		t.Error("Expected saveFunc to be called on timeout")
	}

	t.Logf("Elapsed: %v", elapsed)
}
func TestUpdateStateWithLatestItem_Multiplier(t *testing.T) {
	// Case 1: Small Feed (Feed Count 30 -> Limit 60)
	current1 := FeedState{SeenLinks: make([]string, 40)}
	for i := 0; i < 40; i++ {
		current1.SeenLinks[i] = fmt.Sprintf("old%d", i)
	}

	notifyItems1 := make([]NotificationItem, 30)
	for i := 0; i < 30; i++ {
		notifyItems1[i] = NotificationItem{Link: fmt.Sprintf("new%d", i)}
	}

	next1 := &FeedState{}
	updateStateWithLatestItem(next1, current1, notifyItems1, &gofeed.Feed{}, "", nil, 30)

	expectedLimit1 := 60
	if len(next1.SeenLinks) != expectedLimit1 {
		t.Errorf("Case 1: Expected limit %d, got %d", expectedLimit1, len(next1.SeenLinks))
	}

	// Case 2: Large Feed (Feed Count 200 -> Limit 400)
	current2 := FeedState{SeenLinks: make([]string, 200)}
	for i := 0; i < 200; i++ {
		current2.SeenLinks[i] = fmt.Sprintf("old%d", i)
	}

	notifyItems2 := make([]NotificationItem, 250)
	for i := 0; i < 250; i++ {
		notifyItems2[i] = NotificationItem{Link: fmt.Sprintf("new%d", i)}
	}

	next2 := &FeedState{}
	updateStateWithLatestItem(next2, current2, notifyItems2, &gofeed.Feed{}, "", nil, 200)

	expectedLimit2 := 400
	if len(next2.SeenLinks) != expectedLimit2 {
		t.Errorf("Case 2: Expected limit %d, got %d", expectedLimit2, len(next2.SeenLinks))
	}

	// Case 3: Empty Feed / Error (Feed Count 0 -> No Truncation)
	current3 := FeedState{SeenLinks: make([]string, 500)}
	for i := 0; i < 500; i++ {
		current3.SeenLinks[i] = "item"
	}

	next3 := &FeedState{}
	updateStateWithLatestItem(next3, current3, []NotificationItem{}, &gofeed.Feed{}, "", nil, 0)

	if len(next3.SeenLinks) != 500 {
		t.Errorf("Case 3: Expected no truncation (500), got %d", len(next3.SeenLinks))
	}
}

func TestProcessSingleFeed_NotificationCount(t *testing.T) {
	// Start Mock Server to return this feed
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		feedStr := `<rss version="2.0"><channel><item><title>New Item</title><link>new</link></item></channel></rss>`
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, feedStr)
	}))
	defer ts.Close()

	ctx := context.Background()
	cfg := FeedFilterConfig{}
	state := FeedState{SeenLinks: []string{}}

	// Exec
	_, items, _, err := processSingleFeed(ctx, ts.URL, cfg, nil, &Config{}, state)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Verify
	if len(items) != 1 {
		t.Errorf("Expected 1 notification item, got %d", len(items))
	}
}

func setupFastTest(t *testing.T) {
	t.Helper()

	originalDelay := translationInitialRetryDelay
	translationInitialRetryDelay = 1 * time.Millisecond

	originalTimeout := translationTimeout
	translationTimeout = 50 * time.Millisecond

	// Reset global rate limiter state
	originalLimiter := translationRateLimiter
	translationRateLimiter = &RateLimiter{}

	originalSemaphore := translationSemaphore
	// Drain and recreate semaphore to ensure clean state
	translationSemaphore = make(chan struct{}, translationConcurrency)

	t.Cleanup(func() {
		translationInitialRetryDelay = originalDelay
		translationTimeout = originalTimeout
		translationRateLimiter = originalLimiter
		translationSemaphore = originalSemaphore
	})
}

func TestTranslateNotificationItems_RateLimit(t *testing.T) {
	ctx := context.Background()
	items := []NotificationItem{
		{Title: "Item 1", Description: "Desc 1"},
		{Title: "Item 2", Description: "Desc 2"},
		{Title: "Item 3", Description: "Desc 3"},
	}

	// Mock translator that records timestamp
	var requestTimes []time.Time
	var mu sync.Mutex

	mock := &MockTranslator{
		TranslateFunc: func(ctx context.Context, prompt string) (string, error) {
			mu.Lock()
			requestTimes = append(requestTimes, time.Now())
			mu.Unlock()
			return `{"title": "Translated", "description": "Desc"}`, nil
		},
	}

	start := time.Now()
	translateNotificationItems(ctx, mock, items)
	duration := time.Since(start)

	expectedMinTotal := 2000 * time.Millisecond

	if duration < expectedMinTotal {
		t.Errorf("Total execution time too short. Got %v, expected at least %v", duration, expectedMinTotal)
	}

	// Verify intervals
	mu.Lock()
	defer mu.Unlock()
	if len(requestTimes) != 3 {
		t.Fatalf("Expected 3 requests, got %d", len(requestTimes))
	}

	// Sort times to be sure (though they should be sequential due to mutex in main code)
	sort.Slice(requestTimes, func(i, j int) bool {
		return requestTimes[i].Before(requestTimes[j])
	})

	for i := 0; i < len(requestTimes)-1; i++ {
		diff := requestTimes[i+1].Sub(requestTimes[i])
		if diff < 1000*time.Millisecond {
			t.Errorf("Interval between req %d and %d too short: %v (expected >= 1s)", i, i+1, diff)
		}
	}
}

func TestTranslateNotificationItems_GlobalRateLimit(t *testing.T) {
	setupFastTest(t)
	// Override delay for this specific test
	translationRateLimitDelay = 100 * time.Millisecond

	originalRateLimitDelay := translationRateLimitDelay
	defer func() { translationRateLimitDelay = originalRateLimitDelay }()

	ctx := context.Background()

	// Shared recorder for all goroutines
	var requestTimes []time.Time
	var mu sync.Mutex

	mock := &MockTranslator{
		TranslateFunc: func(ctx context.Context, prompt string) (string, error) {
			mu.Lock()
			requestTimes = append(requestTimes, time.Now())
			mu.Unlock()
			return `{"title": "Translated", "description": "Desc"}`, nil
		},
	}

	// Simulate 3 concurrent feeds being processed
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items := []NotificationItem{{Title: "Item", Description: "Desc"}}
			// Each call creates its own local rate limiter in the current implementation
			translateNotificationItems(ctx, mock, items)
		}()
	}
	wg.Wait()

	duration := time.Since(start)
	t.Logf("Total duration: %v", duration)

	mu.Lock()
	defer mu.Unlock()

	// Sort times
	sort.Slice(requestTimes, func(i, j int) bool {
		return requestTimes[i].Before(requestTimes[j])
	})

	// Check intervals
	minInterval := translationRateLimitDelay - 20*time.Millisecond // Allow small margin
	failures := 0
	for i := 0; i < len(requestTimes)-1; i++ {
		diff := requestTimes[i+1].Sub(requestTimes[i])
		if diff < minInterval {
			t.Logf("Interval between req %d and %d is %v (expected >= %v) - FAILURE", i, i+1, diff, minInterval)
			failures++
		}
	}

	// We expect NO failures if global rate limiting works.
	if failures > 0 {
		t.Errorf("Found %d rate limit violations (Global Rate Limit Broken)", failures)
	}
}
