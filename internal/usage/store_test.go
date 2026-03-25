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
			InputPer1MTokens:      10,
			OutputPer1MTokens:     20,
			CachedInputPer1MToken: 1,
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

func TestCostTableLookupCost(t *testing.T) {
	costs := CostTable{
		"azure_openai:gpt-4o": {InputPer1MTokens: 0.01, OutputPer1MTokens: 0.02},
		"openai:gpt-4o":       {InputPer1MTokens: 0.05, OutputPer1MTokens: 0.10},
		"gpt-4o":              {InputPer1MTokens: 0.03, OutputPer1MTokens: 0.06},
	}

	// Exact match: azure_openai:gpt-4o
	rate, ok := costs.LookupCost("azure_openai", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.01 {
		t.Fatalf("expected exact match for azure_openai:gpt-4o, got ok=%v rate=%+v", ok, rate)
	}

	// Exact match: openai:gpt-4o
	rate, ok = costs.LookupCost("openai", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.05 {
		t.Fatalf("expected exact match for openai:gpt-4o, got ok=%v rate=%+v", ok, rate)
	}

	// Fallback to model-only when endpoint_type doesn't have exact match
	rate, ok = costs.LookupCost("claude", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.03 {
		t.Fatalf("expected fallback match for claude:gpt-4o to model-only, got ok=%v rate=%+v", ok, rate)
	}

	// Fallback when endpoint_type is empty
	rate, ok = costs.LookupCost("", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.03 {
		t.Fatalf("expected fallback match for empty endpoint_type, got ok=%v rate=%+v", ok, rate)
	}

	// Not found
	_, ok = costs.LookupCost("openai", "gpt-unknown")
	if ok {
		t.Fatalf("expected no match for unknown model")
	}
}

func TestStoreRecordWithEndpointType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_events.jsonl")
	store := NewStore(path)

	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	evt := Event{
		Timestamp:    now,
		ClientName:   "alice",
		EndpointType: "  OpenAI  ",
		Model:        "gpt-4o",
		InputTokens:  100,
		OutputTokens: 50,
	}
	if err := store.Record(evt); err != nil {
		t.Fatalf("record event: %v", err)
	}

	listed, err := store.List(Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 event, got %d", len(listed))
	}
	if listed[0].EndpointType != "openai" {
		t.Fatalf("expected endpoint_type normalized to 'openai', got %q", listed[0].EndpointType)
	}

	// Test cost calculation with endpoint_type-aware cost table
	costs := CostTable{
		"openai:gpt-4o":       {InputPer1MTokens: 100, OutputPer1MTokens: 200},
		"azure_openai:gpt-4o": {InputPer1MTokens: 10, OutputPer1MTokens: 20},
		"gpt-4o":              {InputPer1MTokens: 50, OutputPer1MTokens: 100},
	}

	summary, err := store.Summary(now.Add(time.Hour), costs)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	// Should use openai:gpt-4o rates (100 input, 200 output per 1M)
	// Expected cost: 100/1_000_000 * 100 + 50/1_000_000 * 200 = 0.01 + 0.01 = 0.02
	expectedCost := 0.02
	diff := summary.LastHour.EstimatedCost - expectedCost
	if diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("expected estimated cost ~%f, got %f", expectedCost, summary.LastHour.EstimatedCost)
	}
}
