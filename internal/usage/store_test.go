package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRecordListAggregateAndSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_events.jsonl")
	store := NewStore(path)

	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Timestamp: now.Add(-30 * time.Minute), ClientName: "alice", Model: "gpt-4o", InputTokens: 100, OutputTokens: 50, CachedTokens: 10},
		{Timestamp: now.Add(-2 * time.Hour), ClientName: "bob", Model: "gpt-4o", InputTokens: 30, OutputTokens: 20, CachedTokens: 5},
		{Timestamp: now.AddDate(0, 0, -1).Add(2 * time.Hour), ClientName: "alice", Model: "gpt-4o", InputTokens: 70, OutputTokens: 10, CachedTokens: 0},
	}
	for _, evt := range events {
		if err := store.Record(evt); err != nil {
			t.Fatalf("record event: %v", err)
		}
	}

	listed, err := store.List(Filter{ClientName: "alice", Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 alice events, got %d", len(listed))
	}

	costs := CostTable{
		"gpt-4o": {
			InputPer1KTokens:      0.01,
			OutputPer1KTokens:     0.02,
			CachedInputPer1KToken: 0.001,
		},
	}
	from := now.Add(-24 * time.Hour)
	to := now
	agg, err := store.Aggregate(Filter{From: &from, To: &to}, "hour", costs)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.Totals.Requests != 3 {
		t.Fatalf("expected 3 requests in range, got %d", agg.Totals.Requests)
	}
	if agg.Totals.InputTokens != 200 || agg.Totals.OutputTokens != 80 || agg.Totals.CachedTokens != 15 {
		t.Fatalf("unexpected totals: %+v", agg.Totals)
	}

	summary, err := store.Summary(now, costs)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.LastHour.Requests != 1 {
		t.Fatalf("expected last_hour requests=1, got %d", summary.LastHour.Requests)
	}
	if summary.Yesterday.Requests != 1 {
		t.Fatalf("expected yesterday requests=1, got %d", summary.Yesterday.Requests)
	}
}
