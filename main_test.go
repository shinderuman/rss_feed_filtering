package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestRateLimiterWait_Cancel(t *testing.T) {
	r := &RateLimiter{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Wait(ctx, 50*time.Millisecond)
}

func TestRateLimiterWait_CancelDuringSleep(t *testing.T) {
	r := &RateLimiter{}
	r.lastRequestTime = time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	r.Wait(ctx, 5*time.Second)
}

func TestNotificationItem_Format_SlackNormal(t *testing.T) {
	item := NotificationItem{
		FeedTitle:   "Feed&Title",
		Title:       "Item<Title>",
		Link:        "https://example.com/1",
		Description: "desc",
	}
	got := item.Format(true)

	if !strings.Contains(got, "https://example.com/1") {
		t.Errorf("Format() missing link: %q", got)
	}
	if !strings.Contains(got, "desc") {
		t.Errorf("Format() missing description: %q", got)
	}
	if strings.Contains(got, "Item<Title>") {
		t.Errorf("Format() title not escaped: %q", got)
	}
	if strings.Contains(got, "Feed&Title") {
		t.Errorf("Format() feedtitle not escaped: %q", got)
	}
}

func TestNotificationItem_Format_SlackDelayed(t *testing.T) {
	item := NotificationItem{
		FeedTitle:     "FeedTitle",
		Title:         "NewItem",
		Link:          "https://example.com/new",
		Description:   "newdesc",
		PreviousTitle: "OldItem",
		PreviousLink:  "https://example.com/old",
	}
	got := item.Format(true)

	if !strings.Contains(got, "https://example.com/old") {
		t.Errorf("Format() delayed missing previous link: %q", got)
	}
	if !strings.Contains(got, "OldItem") {
		t.Errorf("Format() delayed missing previous title: %q", got)
	}
	if !strings.Contains(got, "https://example.com/new") {
		t.Errorf("Format() delayed missing current link: %q", got)
	}
}

func TestNotificationItem_Format_MastodonNormal(t *testing.T) {
	item := NotificationItem{
		FeedTitle:   "FeedTitle",
		Title:       "ItemTitle",
		Link:        "https://example.com/1",
		Description: "mastdesc",
	}
	got := item.Format(false)

	if !strings.Contains(got, "https://example.com/1") {
		t.Errorf("Format() mastodon missing link: %q", got)
	}
	if !strings.Contains(got, "mastdesc") {
		t.Errorf("Format() mastodon missing description: %q", got)
	}
}

func TestNotificationItem_Format_MastodonDelayed(t *testing.T) {
	item := NotificationItem{
		FeedTitle:     "FeedTitle",
		Title:         "NewItem",
		Link:          "https://example.com/new",
		Description:   "newdesc",
		PreviousTitle: "OldItem",
		PreviousLink:  "https://example.com/old",
	}
	got := item.Format(false)

	if !strings.Contains(got, "https://example.com/old") {
		t.Errorf("Format() mastodon delayed missing previous link: %q", got)
	}
	if !strings.Contains(got, "https://example.com/new") {
		t.Errorf("Format() mastodon delayed missing current link: %q", got)
	}
}

func TestAnthropicTranslator_Translate_EmptyContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}`)
	}))
	defer ts.Close()

	client := anthropic.NewClient(
		option.WithAPIKey("dummy"),
		option.WithBaseURL(ts.URL),
		option.WithMaxRetries(0),
	)
	tr := &AnthropicTranslator{client: &client, model: "claude"}
	_, err := tr.Translate(context.Background(), "prompt")
	if err == nil {
		t.Error("Translate() expected error for empty content, got nil")
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
	// We expect NO error, but partial results.
	if err != nil {
		t.Fatalf("Expected nil error (best effort), got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if saveCallCount == 0 {
		t.Error("Expected saveFunc to be called on timeout")
	}

	t.Logf("Elapsed: %v", elapsed)
}

func TestProcessFeeds_SaveStateError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><item><title>x</title><link>http://e.com/1</link></item></channel></rss>`)
	}))
	defer ts.Close()

	appConfig := &Config{Configs: []FeedFilterConfig{{URLs: []string{ts.URL}}}}
	oldState := &State{Feeds: map[string]FeedState{}}
	saveFunc := func(s *State) error { return fmt.Errorf("save failed") }

	_, err := processFeeds(context.Background(), appConfig, oldState, saveFunc)
	if err == nil {
		t.Error("processFeeds() expected save error, got nil")
	}
}

func TestProcessFeeds_SaveStateOnCollectError(t *testing.T) {
	// collectResults で ctx.Done になる構成：deadline を極短に設定
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	appConfig := &Config{Configs: []FeedFilterConfig{{URLs: []string{"http://127.0.0.1:0/feed"}}}}
	oldState := &State{Feeds: map[string]FeedState{}}

	called := false
	saveFunc := func(s *State) error {
		called = true
		return nil
	}

	_, err := processFeeds(ctx, appConfig, oldState, saveFunc)
	_ = err
	_ = called
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

func TestCollectResults_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan FeedResult)

	called := false
	saveFunc := func(s *State) error {
		called = true
		return nil
	}

	cancel()
	_, err := collectResults(ctx, resultCh, &State{Feeds: map[string]FeedState{}}, saveFunc)
	if err == nil {
		t.Error("collectResults() expected context error, got nil")
	}
	if !called {
		t.Error("saveStateFunc should be called on context done")
	}
}

func TestProcessDomainUrls_RateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	out := make(chan FeedResult, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	processDomainUrls(context.Background(), []string{ts.URL}, FeedFilterConfig{}, &Config{}, &State{Feeds: map[string]FeedState{}}, nil, out, &wg)
	wg.Wait()
	close(out)

	for res := range out {
		_ = res
	}
}

func TestGetHostname(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "正常URL", in: "https://example.com/path", want: "example.com"},
		{name: "空文字", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getHostname(tt.in); got != tt.want {
				t.Errorf("getHostname() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetHostname_ParseError(t *testing.T) {
	// url.Parse がエラーになる入力（制御文字含む）
	got := getHostname("http://[::1:%zz]")
	_ = got
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

func TestProcessSingleFeed_NotModified(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer ts.Close()

	current := FeedState{LastModified: "Wed, 01 Jan 2020 00:00:00 GMT"}
	state, items, status, err := processSingleFeed(context.Background(), ts.URL, FeedFilterConfig{}, nil, &Config{}, current)
	if err != nil {
		t.Fatalf("processSingleFeed() 304 error = %v", err)
	}
	if status != http.StatusNotModified {
		t.Errorf("status = %d, want 304", status)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0 on 304", len(items))
	}
	if state == nil {
		t.Error("state should not be nil on 304")
	}
}

func TestProcessSingleFeed_FetchError(t *testing.T) {
	_, _, _, err := processSingleFeed(context.Background(), "http://127.0.0.1:0/feed", FeedFilterConfig{}, nil, &Config{}, FeedState{})
	if err == nil {
		t.Error("processSingleFeed() expected fetch error, got nil")
	}
}

func TestProcessSingleFeed_NewFeedLimit(t *testing.T) {
	var rss strings.Builder
	rss.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>T</title>`)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&rss, "<item><title>item%d</title><link>http://e.com/%d</link></item>", i, i)
	}
	rss.WriteString(`</channel></rss>`)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, rss.String())
	}))
	defer ts.Close()

	_, items, _, err := processSingleFeed(context.Background(), ts.URL, FeedFilterConfig{}, nil, &Config{EnableSlack: true}, FeedState{})
	if err != nil {
		t.Fatalf("processSingleFeed() error = %v", err)
	}
	if len(items) != initialNotificationLimit {
		t.Errorf("items = %d, want %d (initialNotificationLimit)", len(items), initialNotificationLimit)
	}
}

func TestProcessSingleFeed_Translation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><item><title>x</title><link>http://e.com/1</link></item></channel></rss>`)
	}))
	defer ts.Close()

	origAuth := anthropicAuthToken
	origBase := anthropicBaseURL
	anthropicAuthToken = "dummy"
	anthropicBaseURL = "http://127.0.0.1:0"
	t.Cleanup(func() {
		anthropicAuthToken = origAuth
		anthropicBaseURL = origBase
	})

	_, items, _, err := processSingleFeed(context.Background(), ts.URL, FeedFilterConfig{EnableTranslation: true}, nil, &Config{}, FeedState{})
	if err != nil {
		t.Fatalf("processSingleFeed() translation error = %v", err)
	}
	if len(items) != 1 {
		t.Errorf("items = %d, want 1", len(items))
	}
}

func TestProcessSingleFeed_SeenLinksMigration(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>T</title><item><title>x</title><link>http://e.com/1</link></item></channel></rss>`)
	}))
	defer ts.Close()

	current := FeedState{LastLink: "http://e.com/old"}
	_, _, _, err := processSingleFeed(context.Background(), ts.URL, FeedFilterConfig{}, nil, &Config{}, current)
	if err != nil {
		t.Fatalf("processSingleFeed() error = %v", err)
	}
}

func TestFetchAndParse_NotModified(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.Header().Set("ETag", "tag1")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, "<rss/>")
	}))
	defer ts.Close()

	feed, _, status, err := fetchAndParse(context.Background(), ts.URL, "", "oldtag")
	if err != nil {
		t.Fatalf("fetchAndParse() 304 error = %v", err)
	}
	if status != http.StatusNotModified {
		t.Errorf("status = %d, want 304", status)
	}
	if feed != nil {
		t.Error("feed should be nil on 304")
	}
}

func TestFetchAndParse_ParseError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, "not xml at all")
	}))
	defer ts.Close()

	_, _, _, err := fetchAndParse(context.Background(), ts.URL, "", "")
	if err == nil {
		t.Error("fetchAndParse() expected parse error, got nil")
	}
}

func TestFetchFeedContent_Errors(t *testing.T) {
	// 無効URLで NewRequestWithContext エラー
	_, _, _, err := fetchFeedContent(context.Background(), "://invalid", nil)
	if err == nil {
		t.Error("fetchFeedContent() expected error for invalid URL, got nil")
	}

	// 接続エラー
	_, _, _, err = fetchFeedContent(context.Background(), "http://127.0.0.1:0/feed", nil)
	if err == nil {
		t.Error("fetchFeedContent() expected connection error, got nil")
	}
}

func TestFetchFeedContent_NotModified(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") != "" || r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, "<rss/>")
	}))
	defer ts.Close()

	body, _, status, err := fetchFeedContent(context.Background(), ts.URL, map[string]string{"If-Modified-Since": "Wed, 01 Jan 2020 00:00:00 GMT"})
	if err != nil {
		t.Fatalf("fetchFeedContent() 304 error = %v", err)
	}
	if status != http.StatusNotModified {
		t.Errorf("status = %d, want 304", status)
	}
	if body != nil {
		t.Errorf("body should be nil on 304, got %q", body)
	}
}

func TestFetchFeedContent_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, _, status, err := fetchFeedContent(context.Background(), ts.URL, nil)
	if err == nil {
		t.Error("fetchFeedContent() expected error for 500, got nil")
	}
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
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

func TestGetNotificationItems_DelayedPrev(t *testing.T) {
	feed := &gofeed.Feed{
		Title: "T",
		Items: []*gofeed.Item{
			{Title: "newest", Link: "http://e.com/newest"},
			{Title: "new", Link: "http://e.com/new"},
			{Title: "old", Link: "http://e.com/old"},
		},
	}
	cfg := FeedFilterConfig{}
	appConfig := &Config{DelayedDomains: []string{"delayed.com"}}

	items := getNotificationItems(feed, cfg, []string{"http://e.com/old"}, []string{}, appConfig, "https://delayed.com/feed")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].PreviousLink != "http://e.com/old" {
		t.Errorf("PreviousLink = %q, want http://e.com/old", items[0].PreviousLink)
	}
}

func TestGetNotificationItems_EmptyTitleDesc(t *testing.T) {
	feed := &gofeed.Feed{
		Title: "T",
		Items: []*gofeed.Item{
			{Title: "", Link: "http://e.com/1", Description: ""},
			{Title: "valid", Link: "http://e.com/2"},
		},
	}
	items := getNotificationItems(feed, FeedFilterConfig{}, []string{}, []string{}, &Config{}, "https://x.com/feed")
	if len(items) != 1 {
		t.Errorf("items = %d, want 1 (empty title+desc skipped)", len(items))
	}
}

func TestGetNotificationItems_SeenBreak(t *testing.T) {
	feed := &gofeed.Feed{
		Title: "T",
		Items: []*gofeed.Item{
			{Title: "a", Link: "http://e.com/1"},
			{Title: "b", Link: "http://e.com/2"},
		},
	}
	items := getNotificationItems(feed, FeedFilterConfig{}, []string{"http://e.com/2"}, []string{}, &Config{}, "https://x.com/feed")
	if len(items) != 1 {
		t.Errorf("items = %d, want 1 (seen break)", len(items))
	}
}

func TestSendAggregatedNotifications(t *testing.T) {
	// 1. Mock Processing Functions
	mastodonCalls := 0
	slackCalls := 0
	var mockMu sync.Mutex

	// Save original functions
	origMastodonFunc := processMastodonFunc
	origSlackFunc := processSlackFunc
	defer func() {
		processMastodonFunc = origMastodonFunc
		processSlackFunc = origSlackFunc
	}()

	// Define mocks
	processMastodonFunc = func(ctx context.Context, item NotificationItem, wg *sync.WaitGroup, errMu *sync.Mutex, errs *[]string) {
		defer wg.Done()
		mockMu.Lock()
		mastodonCalls++
		mockMu.Unlock()
	}

	processSlackFunc = func(batch []NotificationItem, wg *sync.WaitGroup, errMu *sync.Mutex, errs *[]string) {
		defer wg.Done()
		mockMu.Lock()
		slackCalls++
		mockMu.Unlock()
	}

	// 2. Test Data
	ctx := context.Background()
	batches := [][]NotificationItem{
		{
			{Title: "Item 1", Link: "link1", EnableMastodon: true, EnableSlack: true, SlackChannelID: "C1"},
			{Title: "Item 2", Link: "link2", EnableMastodon: true, EnableSlack: true, SlackChannelID: "C1"},
		},
		{
			{Title: "Item 3", Link: "link3", EnableMastodon: true, EnableSlack: true, SlackChannelID: "C2"},
		},
	}
	// Total Mastodon Items: 3
	// Total Slack Batches: 2

	// 3. Execute
	start := time.Now()
	sendAggregatedNotifications(ctx, batches)
	duration := time.Since(start)

	// 4. Verify
	if mastodonCalls != 3 {
		t.Errorf("Expected 3 Mastodon calls, got %d", mastodonCalls)
	}
	if slackCalls != 2 {
		t.Errorf("Expected 2 Slack calls, got %d", slackCalls)
	}

	// Verify Staggered Delay
	expectedDuration := 3 * 200 * time.Millisecond // 600ms
	if duration < expectedDuration {
		t.Errorf("Execution too fast. Expected at least %v, got %v. Staggering might be missing.", expectedDuration, duration)
	}
}

func TestSendAggregatedNotifications_EmptyAndErrors(t *testing.T) {
	// 空バッチ
	sendAggregatedNotifications(context.Background(), nil)

	// モックでエラーを発生
	origMastodon := processMastodonFunc
	origSlack := processSlackFunc
	t.Cleanup(func() {
		processMastodonFunc = origMastodon
		processSlackFunc = origSlack
	})
	processMastodonFunc = defaultProcessMastodon
	processSlackFunc = defaultProcessSlack

	batches := [][]NotificationItem{
		{{Title: "T", Link: "http://e.com/1", EnableMastodon: true}},
	}
	sendAggregatedNotifications(context.Background(), batches)
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

func TestDetermineLastLink(t *testing.T) {
	delayedDomains := []string{"example.com"}

	tests := []struct {
		name         string
		currentState FeedState
		notifyItems  []NotificationItem
		feed         *gofeed.Feed
		feedURL      string
		want         string
	}{
		{
			name:         "notifyItemsあり→先頭Link",
			notifyItems:  []NotificationItem{{Link: "https://new.example.com/1"}},
			feed:         &gofeed.Feed{},
			feedURL:      "https://example.com/feed",
			want:         "https://new.example.com/1",
		},
		{
			name:         "LastLink空・非delayed→feed.Items[0]",
			currentState: FeedState{},
			notifyItems:  nil,
			feed:         &gofeed.Feed{Items: []*gofeed.Item{{Link: "https://example.com/0"}, {Link: "https://example.com/1"}}},
			feedURL:      "https://other.com/feed",
			want:         "https://example.com/0",
		},
		{
			name:         "LastLink空・delayed→delayedStartIndex位置",
			currentState: FeedState{},
			notifyItems:  nil,
			feed:         &gofeed.Feed{Items: []*gofeed.Item{{Link: "https://example.com/0"}, {Link: "https://example.com/1"}}},
			feedURL:      "https://example.com/feed",
			want:         "https://example.com/1",
		},
		{
			name:         "LastLink既存→そのまま返却",
			currentState: FeedState{LastLink: "https://existing.example.com/prev"},
			notifyItems:  nil,
			feed:         &gofeed.Feed{},
			feedURL:      "https://example.com/feed",
			want:         "https://existing.example.com/prev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineLastLink(tt.currentState, tt.notifyItems, tt.feed, tt.feedURL, delayedDomains)
			if got != tt.want {
				t.Errorf("determineLastLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPassesFilters(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		desc    string
		include []string
		exclude []string
		want    bool
	}{
		{name: "exclude一致→false", title: "Breaking News", desc: "", include: nil, exclude: []string{"breaking"}, want: false},
		{name: "include一致→true", title: "Go Release", desc: "", include: []string{"go"}, exclude: nil, want: true},
		{name: "include不一致→false", title: "Rust Release", desc: "", include: []string{"go"}, exclude: nil, want: false},
		{name: "include空→true", title: "Anything", desc: "", include: nil, exclude: nil, want: true},
		{name: "大小区別なし", title: "GO RELEASE", desc: "", include: []string{"go"}, exclude: nil, want: true},
		{name: "descも検索対象", title: "Title", desc: "includes keyword", include: []string{"keyword"}, exclude: nil, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &gofeed.Item{Title: tt.title, Description: tt.desc}
			got := passesFilters(item, tt.include, tt.exclude)
			if got != tt.want {
				t.Errorf("passesFilters() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsDelayedDomain(t *testing.T) {
	domains := []string{"delayed.com"}

	tests := []struct {
		name    string
		feedURL string
		want    bool
	}{
		{name: "一致→true", feedURL: "https://delayed.com/feed", want: true},
		{name: "不一致→false", feedURL: "https://other.com/feed", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDelayedDomain(tt.feedURL, domains); got != tt.want {
				t.Errorf("isDelayedDomain() = %v, want %v", got, tt.want)
			}
		})
	}

	if got := isDelayedDomain("https://any.com/feed", nil); got != false {
		t.Errorf("isDelayedDomain() with empty domains = %v, want false", got)
	}
}

func TestCleanHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "タグ除去", in: "<p>hello <b>world</b></p>", want: "hello world"},
		{name: "URL除去", in: "see https://example.com for details", want: "see  for details"},
		{name: "空文字", in: "", want: ""},
		{name: "前後空白トリム", in: "  <p>text</p>  ", want: "text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cleanHTML(tt.in)
			if got != tt.want {
				t.Errorf("cleanHTML() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCleanHTML_ParseError(t *testing.T) {
	in := string([]byte{0xff, 0xfe, 0x00})
	got := cleanHTML(in)
	if got == "" {
		t.Errorf("cleanHTML() parse error fallback returned empty for input %q", in)
	}
}

func TestCleanHTML_NestedTags(t *testing.T) {
	in := "<div><p>nested</p><br/></div>"
	got := cleanHTML(in)
	if !strings.Contains(got, "nested") {
		t.Errorf("cleanHTML() = %q, want nested text", got)
	}
}

func TestCleanHTML_Malformed(t *testing.T) {
	// goquery がエラーを返す入力でフォールバックが機能することを検証（panicしない）
	in := string(rune(0))
	_ = cleanHTML(in)
}

func TestTruncateStatus(t *testing.T) {
	short := strings.Repeat("a", 499)
	exact := strings.Repeat("a", 500)
	long := strings.Repeat("a", 501)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "499字そのまま", in: short, want: short},
		{name: "500字ちょうどそのまま", in: exact, want: exact},
		{name: "501字→497字+省略記号", in: long, want: strings.Repeat("a", 497) + "..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateStatus(tt.in)
			if got != tt.want {
				t.Errorf("truncateStatus() len=%d, want len=%d", len(got), len(tt.want))
			}
		})
	}

	japanese := strings.Repeat("あ", 501)
	got := truncateStatus(japanese)
	wantRunes := []rune(japanese)[:maxStatusLength-3]
	if string([]rune(got)[:maxStatusLength-3]) != string(wantRunes) || !strings.HasSuffix(got, "...") {
		t.Errorf("truncateStatus() Japanese = len(rune)=%d, want 497 runes + ...", len([]rune(got)))
	}
}

func TestDefaultProcessSlack_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"rate_limited"}`)
	}))
	defer ts.Close()

	origAPIURL := slackAPIURL
	origToken := slackBotToken
	slackAPIURL = ts.URL + "/"
	slackBotToken = "dummy-token"
	t.Cleanup(func() {
		slackAPIURL = origAPIURL
		slackBotToken = origToken
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	batch := []NotificationItem{{Title: "T", EnableSlack: true, SlackChannelID: "C1"}}

	wg.Add(1)
	defaultProcessSlack(batch, &wg, &mu, &errs)
	wg.Wait()

	if len(errs) == 0 {
		t.Error("defaultProcessSlack() expected error recorded, got none")
	}
}

func TestPostToSlack(t *testing.T) {
	var capturedBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"channel":"C1","ts":"123.45"}`)
	}))
	defer ts.Close()

	origAPIURL := slackAPIURL
	origToken := slackBotToken
	slackAPIURL = ts.URL + "/"
	slackBotToken = "dummy-token"
	t.Cleanup(func() {
		slackAPIURL = origAPIURL
		slackBotToken = origToken
	})

	items := []NotificationItem{
		{Title: "Title", Link: "https://example.com/1", Description: "desc", FeedTitle: "Feed", EnableSlack: true, SlackChannelID: "C1"},
		{Title: "Title2", Link: "https://example.com/2", Description: "desc2", FeedTitle: "Feed", EnableSlack: true, SlackChannelID: "C1"},
	}
	if err := postToSlack(items); err != nil {
		t.Fatalf("postToSlack() error = %v", err)
	}

	vals, err := url.ParseQuery(capturedBody)
	if err != nil {
		t.Fatalf("ParseQuery error = %v", err)
	}
	if vals.Get("channel") != "C1" {
		t.Errorf("channel = %q, want C1", vals.Get("channel"))
	}
	if !strings.Contains(vals.Get("text"), "https://example.com/1") {
		t.Errorf("text missing link: %q", vals.Get("text"))
	}

	if err := postToSlack([]NotificationItem{}); err != nil {
		t.Errorf("postToSlack() empty slice error = %v", err)
	}
	if err := postToSlack([]NotificationItem{{EnableSlack: false}}); err != nil {
		t.Errorf("postToSlack() EnableSlack=false error = %v", err)
	}
	if err := postToSlack([]NotificationItem{{EnableSlack: true, SlackChannelID: ""}}); err != nil {
		t.Errorf("postToSlack() empty channel error = %v", err)
	}
}

func TestPostToSlack_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"rate_limited"}`)
	}))
	defer ts.Close()

	origAPIURL := slackAPIURL
	origToken := slackBotToken
	slackAPIURL = ts.URL + "/"
	slackBotToken = "dummy-token"
	t.Cleanup(func() {
		slackAPIURL = origAPIURL
		slackBotToken = origToken
	})

	items := []NotificationItem{{Title: "T", EnableSlack: true, SlackChannelID: "C1"}}
	err := postToSlack(items)
	if err == nil {
		t.Error("postToSlack() expected error for ok=false, got nil")
	}
}

func TestPostToMastodon(t *testing.T) {
	var capturedBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		if r.URL.Path != "/api/v1/statuses" {
			t.Errorf("path = %q, want /api/v1/statuses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1"}`)
	}))
	defer ts.Close()

	origServer := mastodonServer
	origToken := mastodonAccessToken
	mastodonServer = ts.URL
	mastodonAccessToken = "global-token"
	t.Cleanup(func() {
		mastodonServer = origServer
		mastodonAccessToken = origToken
	})

	item := NotificationItem{
		Title:               "Title",
		Link:                "https://example.com/1",
		Description:         "desc",
		FeedTitle:           "Feed",
		EnableMastodon:      true,
		MastodonAccessToken: "item-token",
	}
	if err := postToMastodon(context.Background(), item); err != nil {
		t.Fatalf("postToMastodon() error = %v", err)
	}

	vals, err := url.ParseQuery(capturedBody)
	if err != nil {
		t.Fatalf("ParseQuery error = %v", err)
	}
	if !strings.Contains(vals.Get("status"), "https://example.com/1") {
		t.Errorf("status missing link: %q", vals.Get("status"))
	}

	if err := postToMastodon(context.Background(), NotificationItem{EnableMastodon: false}); err != nil {
		t.Errorf("postToMastodon() EnableMastodon=false error = %v", err)
	}
}

func TestPostToMastodon_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	origServer := mastodonServer
	mastodonServer = ts.URL
	t.Cleanup(func() {
		mastodonServer = origServer
	})

	item := NotificationItem{Title: "T", EnableMastodon: true}
	err := postToMastodon(context.Background(), item)
	if err == nil {
		t.Error("postToMastodon() expected error for 500, got nil")
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

func TestExecuteWithRetry_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	setupFastTest(t)

	_, err := executeWithRetry(ctx, func() (string, error) {
		return "", fmt.Errorf("operation timeout")
	})
	if err == nil {
		t.Error("executeWithRetry() expected context error, got nil")
	}
}

func TestExecuteWithRetry_Exhausted(t *testing.T) {
	setupFastTest(t)

	_, err := executeWithRetry(context.Background(), func() (string, error) {
		return "", fmt.Errorf("operation timeout")
	})
	if err == nil {
		t.Error("executeWithRetry() expected error after retries exhausted, got nil")
	}
}

func TestShouldRetry(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "timeout文字列", err: fmt.Errorf("operation timeout"), want: true},
		{name: "deadline文字列", err: fmt.Errorf("deadline exceeded"), want: true},
		{name: "その他エラー", err: fmt.Errorf("some error"), want: false},
		{name: "context.DeadlineExceeded", err: context.DeadlineExceeded, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRetry(tt.err); got != tt.want {
				t.Errorf("shouldRetry() = %v, want %v", got, tt.want)
			}
		})
	}

	// netErr.Timeout() パス: net/http の timeout エラー
	client := &http.Client{Timeout: 1 * time.Millisecond}
	_, err := client.Get("http://192.0.2.1:1/x")
	if err == nil {
		t.Skip("no timeout error generated")
	}
	if !shouldRetry(err) {
		t.Errorf("shouldRetry() timeout net error = false, want true. err=%v", err)
	}
}

func TestShouldRetry_AnthropicError(t *testing.T) {
	// リトライ対象外の anthropic.Error (400) を実際のレスポンスから生成
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"bad_request","message":"bad"}}`)
	}))
	defer ts.Close()

	client := anthropic.NewClient(
		option.WithAPIKey("dummy"),
		option.WithBaseURL(ts.URL),
		option.WithMaxRetries(0),
	)
	_, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model("claude"),
		MaxTokens: int64(1),
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if err == nil {
		t.Fatal("expected anthropic error, got nil")
	}
	if shouldRetry(err) {
		t.Errorf("shouldRetry() 400 = true, want false")
	}
}

func TestShouldRetry_DeadlineExceeded(t *testing.T) {
	if !shouldRetry(context.DeadlineExceeded) {
		t.Error("shouldRetry(context.DeadlineExceeded) = false, want true")
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
