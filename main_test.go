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

func TestAnthropicTranslator_Translate_TimeoutActual(t *testing.T) {
	// 1. Mock server that sleeps longer than 10s (timeout)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(translationTimeout + 2*time.Second)
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
	// context.Background() passed here, so reliance is on Translate's internal context.WithTimeout (if present)
	// OR http.Client timeout if context.WithTimeout was reverted.
	// Since user reverted context.WithTimeout, this verifies http.Client timeout works locally.
	_, err := translator.Translate(context.Background(), "test")
	duration := time.Since(start)

	// 4. Verification
	if err == nil {
		t.Fatal("Expected timeout error, got nil")
	}

	// Should be slightly over 10s
	if duration > translationTimeout+1*time.Second {
		t.Errorf("Test took too long: %v. Expected around %v", duration, translationTimeout)
	}

	t.Logf("Timeout confirmed. Duration: %v, Error: %v", duration, err)
}
