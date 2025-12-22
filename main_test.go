package main

import (
	"reflect"
	"testing"

	"github.com/mmcdole/gofeed"
)

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
