package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
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

func TestTranslateNotificationItems(t *testing.T) {
	ctx := context.Background()
	items := []NotificationItem{
		{Title: "Original Title", Description: "Original Description"},
	}

	// Test Success Case
	mock := &MockTranslator{
		TranslateFunc: func(ctx context.Context, prompt string) (string, error) {
			if strings.Contains(prompt, "Title:") {
				return "翻訳されたタイトル", nil
			}
			if strings.Contains(prompt, "Description:") {
				return "翻訳された説明", nil
			}
			return "", fmt.Errorf("unknown prompt")
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

	// Test Error Case (Fallback)
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
	if fallbackItems[0].Description != "Original Description" {
		t.Errorf("Expected fallback description 'Original Description', got '%s'", fallbackItems[0].Description)
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

	start := time.Now()
	newState, err := processFeeds(ctx, appConfig, oldState)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("processFeeds failed: %v", err)
	}

	if len(newState.Feeds) != 2 {
		t.Errorf("Expected 2 feed states, got %d", len(newState.Feeds))
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

	start := time.Now()
	newState, err := processFeeds(ctx, appConfig, oldState)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("processFeeds failed: %v", err)
	}

	if len(newState.Feeds) != 2 {
		t.Errorf("Expected 2 feed states, got %d", len(newState.Feeds))
	}

	// Verify time
	// If grouped (same domain): sequential -> 800ms
	// If separated (diff domain): parallel -> 400ms
	if elapsed >= 600*time.Millisecond {
		t.Errorf("Execution took %v, expected less than 600ms (inner parallel execution failed)", elapsed)
	}
}
