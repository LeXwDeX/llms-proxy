package usage

import (
	"sort"
	"strings"
	"time"
)

// Event is one usage record item.
type Event struct {
	Timestamp           time.Time `json:"timestamp"`
	ClientName          string    `json:"client_name"`
	EndpointType        string    `json:"endpoint_type,omitempty"`
	Model               string    `json:"model,omitempty"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CachedTokens        int64     `json:"cached_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens,omitempty"`
	RequestID           string    `json:"request_id,omitempty"`
	Target              string    `json:"target,omitempty"`
	Path                string    `json:"path,omitempty"`
	StatusCode          int       `json:"status_code"`
}

// Filter controls list/aggregate query range.
type Filter struct {
	From       *time.Time
	To         *time.Time
	ClientName string
	Model      string
	Limit      int
}

// CostRates maps model to token prices (per 1M tokens).
type CostRates struct {
	InputPer1MTokens      float64
	OutputPer1MTokens     float64
	CachedInputPer1MToken float64
	CacheReadPer1MToken   float64
}

// CostTable holds rates keyed by model name.
type CostTable map[string]CostRates

// LookupCost finds cost rates by model name only.
func (ct CostTable) LookupCost(model string) (CostRates, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return CostRates{}, false
	}
	rates, ok := ct[model]
	return rates, ok
}

// Totals is aggregated usage and estimated cost.
type Totals struct {
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	EstimatedCost       float64 `json:"estimated_cost"`
}

// Bucket is one time-bucket usage aggregation.
type Bucket struct {
	BucketStart time.Time `json:"bucket_start"`
	BucketEnd   time.Time `json:"bucket_end"`
	Totals
}

// DimensionTotals is one dimension aggregate item.
type DimensionTotals struct {
	Key string `json:"key"`
	Totals
}

// AggregateResult is the full aggregation output.
type AggregateResult struct {
	From     time.Time         `json:"from"`
	To       time.Time         `json:"to"`
	GroupBy  string            `json:"group_by"`
	Totals   Totals            `json:"totals"`
	Buckets  []Bucket          `json:"buckets"`
	ByClient []DimensionTotals `json:"by_client"`
	ByModel  []DimensionTotals `json:"by_model"`
}

// SummaryResult provides predefined windows for UI.
type SummaryResult struct {
	GeneratedAt time.Time `json:"generated_at"`
	LastHour    Totals    `json:"last_hour"`
	Today       Totals    `json:"today"`
	Yesterday   Totals    `json:"yesterday"`
	Last7Days   Totals    `json:"last_7_days"`
	Last30Days  Totals    `json:"last_30_days"`
}

// Recorder records one usage event.
type Recorder interface {
	Record(event Event) error
}

// --- Utility functions used by nosql.UsageStore and other aggregation code ---

// FilterEvents filters events according to filter criteria.
func FilterEvents(events []Event, filter Filter) []Event {
	clientKey := strings.TrimSpace(filter.ClientName)
	modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

	filtered := make([]Event, 0, len(events))
	for _, evt := range events {
		t := evt.Timestamp.UTC()
		if filter.From != nil && t.Before(filter.From.UTC()) {
			continue
		}
		if filter.To != nil && t.After(filter.To.UTC()) {
			continue
		}
		if clientKey != "" && evt.ClientName != clientKey {
			continue
		}
		if modelKey != "" && strings.ToLower(evt.Model) != modelKey {
			continue
		}
		filtered = append(filtered, evt)
	}
	return filtered
}

// NormalizeGroupBy normalises group_by values.
func NormalizeGroupBy(groupBy string) string {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	if groupBy != "hour" {
		return "day"
	}
	return groupBy
}

// BucketStartFor computes the bucket boundary for the given time.
func BucketStartFor(t time.Time, groupBy string) time.Time {
	t = t.UTC()
	if groupBy == "hour" {
		return t.Truncate(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// StepDuration returns the duration for one bucket.
func StepDuration(groupBy string) time.Duration {
	if groupBy == "hour" {
		return time.Hour
	}
	return 24 * time.Hour
}

// SortedDimensions converts a dimension map to a sorted slice.
func SortedDimensions(input map[string]*DimensionTotals) []DimensionTotals {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]DimensionTotals, 0, len(keys))
	for _, key := range keys {
		result = append(result, *input[key])
	}
	return result
}

// AddEventTotals adds one event's contribution to aggregate totals.
func AddEventTotals(target *Totals, evt Event, costs CostTable) {
	target.Requests++
	target.InputTokens += evt.InputTokens
	target.OutputTokens += evt.OutputTokens
	target.CachedTokens += evt.CachedTokens
	target.CacheCreationTokens += evt.CacheCreationTokens
	target.EstimatedCost += EstimateEventCost(evt, costs)
}

// EstimateEventCost returns the estimated cost for a single event.
func EstimateEventCost(evt Event, costs CostTable) float64 {
	rate, ok := costs.LookupCost(evt.Model)
	if !ok {
		return 0
	}
	return float64(evt.InputTokens)/1_000_000*rate.InputPer1MTokens +
		float64(evt.OutputTokens)/1_000_000*rate.OutputPer1MTokens +
		float64(evt.CacheCreationTokens)/1_000_000*rate.CachedInputPer1MToken +
		float64(evt.CachedTokens)/1_000_000*rate.CacheReadPer1MToken
}
