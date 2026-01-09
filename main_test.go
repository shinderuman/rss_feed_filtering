package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	// Mock maxSeenLinks for test?
	// It's a const in main.go, hard to mock directly without changing code structure.
	// But we can test basic appending.
	// We can trust the const is 100. If we want to test truncation, we need 101 items.

	updateStateWithLatestItem(nextState, current, notifyItems, feed, "", nil)

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

	items := getNotificationItems(feed, cfg, seenLinks, nil, nil, "")

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

	items := getNotificationItems(feed, cfg, seenLinks, nil, nil, "")

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

	items := getNotificationItems(feed, cfg, seenLinks, nil, nil, "")

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
	err := processFeeds(ctx, appConfig, oldState, saveFunc)
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
	// URL1: 127.0.0.1 (default ts.URL)
	// URL2: localhost (modified)
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
	err := processFeeds(ctx, appConfig, oldState, saveFunc)
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

	// 3. Run
	err := processFeeds(ctx, appConfig, oldState, saveFunc)
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

	err := processFeeds(ctx, appConfig, oldState, saveFunc)
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

	if err := processFeeds(ctx, appConfig, state1, saveFunc); err != nil {
		t.Fatalf("Run 1 failed: %v", err)
	}

	// Verify Run 1 State
	key := fmt.Sprintf("%x", md5.Sum([]byte(ts.URL)))
	if len(savedState.Feeds[key].SeenLinks) != 1 {
		t.Fatalf("Run 1: Expected 1 seen link, got %d", len(savedState.Feeds[key].SeenLinks))
	}

	// Run 2: Re-run with Saved State
	// The feed content is exactly the same. Should produce NO notifications and maintain state.
	// (Note: In real app, we track LastLink/SeenLinks. processSingleFeed should see link is in SeenLinks and produce 0 notifyItems)

	// We need to capture if any notifications were sent.
	// Since processFeeds calls processSingleFeed which calls sendNotificationItems (which logs),
	// we can't easily capture output here without refactoring sendNotificationItems injection.
	// BUT, we can check the State. If 0 items were found, LastLink/SeenLinks shouldn't change (or just be re-asserted).

	state2 := savedState
	var savedState2 *State
	saveFunc2 := func(s *State) error {
		savedState2 = s
		return nil
	}

	if err := processFeeds(ctx, appConfig, state2, saveFunc2); err != nil {
		t.Fatalf("Run 2 failed: %v", err)
	}

	// If processFeeds found 0 new items to notify, it still runs and might save state (e.g. LastModified might update if server doesn't support 304).
	// But SeenLinks should NOT grow.
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

	items := getNotificationItems(feed, cfg, seenLinks, nil, nil, "")

	if len(items) != 1 {
		t.Errorf("Expected 1 item, got %d", len(items))
	}
	if items[0].MastodonAccessToken != "custom_token" {
		t.Errorf("Expected token 'custom_token', got '%s'", items[0].MastodonAccessToken)
	}
}

func TestAnthropicTranslator_Translate_Timeout_Retry(t *testing.T) {
	// 1. Mock server that sleeps longer than 10s (timeout) for first 2 attempts, then succeeds
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

	// Expected Timing:
	// Attempt 1 (0s): Timeout (10s) -> Sleep 3s
	// Attempt 2 (13s): Timeout (10s) -> Sleep 6s
	// Attempt 3 (29s): Success (immediate)
	// Total minimum time: 29s
	// However, if the http.Client timeout fires at 10s, the "Wait" in executeWithRetry happens AFTER the error return.
	// So:
	// 1. Call -> 10s -> Timeout Err -> Sleep 3s
	// 2. Call -> 10s -> Timeout Err -> Sleep 6s
	// 3. Call -> Success
	// Total: 10 + 3 + 10 + 6 = 29s approximately.

	expectedMin := 20 * time.Second // Conservative lower bound
	if duration < expectedMin {
		t.Errorf("Test took too short: %v. Expected > %v", duration, expectedMin)
	}

	t.Logf("Retry success confirmed. Duration: %v, Attempts: %d", duration, attempts)
}

func TestAnthropicTranslator_Translate_Retry429(t *testing.T) {
	// 1. Mock server that returns 429 twice, then 200
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

	// Expected Timing:
	// Attempt 1 (0s): 429 -> Sleep 3s
	// Attempt 2 (3s): 429 -> Sleep 6s
	// Attempt 3 (9s): 200 -> Return
	// Total minimum time: 9s
	expectedMin := 9 * time.Second
	if duration < expectedMin {
		t.Errorf("Expected duration >= %v, got %v", expectedMin, duration)
	}
}

func TestAnthropicTranslator_Translate_Retry502(t *testing.T) {
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

	// Expected Timing:
	// Attempt 1 (0s): 502 -> Sleep 3s
	// Attempt 2 (3s): 502 -> Sleep 6s
	// Attempt 3 (9s): 200 -> Return
	// Total minimum time: 9s
	expectedMin := 9 * time.Second
	if duration < expectedMin {
		t.Errorf("Expected duration >= %v, got %v", expectedMin, duration)
	}
}
