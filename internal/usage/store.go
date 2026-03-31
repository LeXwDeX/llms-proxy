package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event is one usage record item.
type Event struct {
	Timestamp    time.Time `json:"timestamp"`
	ClientName   string    `json:"client_name"`
	EndpointType string    `json:"endpoint_type,omitempty"`
	Model        string    `json:"model,omitempty"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CachedTokens int64     `json:"cached_tokens"`
	RequestID    string    `json:"request_id,omitempty"`
	Target       string    `json:"target,omitempty"`
	Path         string    `json:"path,omitempty"`
	StatusCode   int       `json:"status_code"`
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
}

// CostTable holds rates by "endpoint_type:model" key, falling back to "model" for backward compat.
type CostTable map[string]CostRates

// LookupCost finds cost rates for an event, trying "endpoint_type:model" first, then "model".
func (ct CostTable) LookupCost(endpointType, model string) (CostRates, bool) {
	endpointType = strings.ToLower(strings.TrimSpace(endpointType))
	model = strings.ToLower(strings.TrimSpace(model))

	// Exact match: endpoint_type:model
	if endpointType != "" {
		if rates, ok := ct[endpointType+":"+model]; ok {
			return rates, true
		}
	}
	// Fallback: model only
	if rates, ok := ct[model]; ok {
		return rates, true
	}
	return CostRates{}, false
}

// Totals is aggregated usage and estimated cost.
type Totals struct {
	Requests      int64   `json:"requests"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CachedTokens  int64   `json:"cached_tokens"`
	EstimatedCost float64 `json:"estimated_cost"`
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

// Store persists usage events in a JSONL file.
type Store struct {
	mu   sync.RWMutex
	path string
}

// NewStore creates a usage JSONL store.
func NewStore(path string) *Store {
	return &Store{path: strings.TrimSpace(path)}
}

// Path returns current JSONL path.
func (s *Store) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// SetPath updates JSONL path.
func (s *Store) SetPath(path string) {
	s.mu.Lock()
	s.path = strings.TrimSpace(path)
	s.mu.Unlock()
}

// Record appends one JSON line event.
func (s *Store) Record(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := strings.TrimSpace(s.path)
	if path == "" {
		return errors.New("usage events file path is empty")
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.ClientName = strings.TrimSpace(event.ClientName)
	event.EndpointType = strings.ToLower(strings.TrimSpace(event.EndpointType))
	event.Model = strings.ToLower(strings.TrimSpace(event.Model))
	event.RequestID = strings.TrimSpace(event.RequestID)
	event.Target = strings.TrimSpace(event.Target)
	event.Path = strings.TrimSpace(event.Path)

	if err := ensureJSONLFile(path); err != nil {
		return err
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal usage event: %w", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open usage events file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("append usage event: %w", err)
	}
	return nil
}

// List returns events sorted by timestamp desc.
func (s *Store) List(filter Filter) ([]Event, error) {
	events, err := s.readAll()
	if err != nil {
		return nil, err
	}

	filtered := filterEvents(events, filter)
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Timestamp.After(filtered[j].Timestamp)
	})

	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

// Aggregate aggregates usage in requested time buckets.
func (s *Store) Aggregate(filter Filter, groupBy string, costs CostTable) (AggregateResult, error) {
	events, err := s.readAll()
	if err != nil {
		return AggregateResult{}, err
	}

	events = filterEvents(events, filter)
	groupBy = normalizeGroupBy(groupBy)

	result := AggregateResult{GroupBy: groupBy}
	if filter.From != nil {
		result.From = filter.From.UTC()
	}
	if filter.To != nil {
		result.To = filter.To.UTC()
	}

	bucketMap := map[time.Time]*Bucket{}
	byClient := map[string]*DimensionTotals{}
	byModel := map[string]*DimensionTotals{}

	for _, evt := range events {
		addEventTotals(&result.Totals, evt, costs)

		bucketStart := bucketStartFor(evt.Timestamp.UTC(), groupBy)
		bucket, ok := bucketMap[bucketStart]
		if !ok {
			bucket = &Bucket{
				BucketStart: bucketStart,
				BucketEnd:   bucketStartFor(bucketStart.Add(stepDuration(groupBy)), groupBy),
			}
			bucketMap[bucketStart] = bucket
		}
		addEventTotals(&bucket.Totals, evt, costs)

		clientKey := evt.ClientName
		if clientKey == "" {
			clientKey = "unknown"
		}
		clientDim := byClient[clientKey]
		if clientDim == nil {
			clientDim = &DimensionTotals{Key: clientKey}
			byClient[clientKey] = clientDim
		}
		addEventTotals(&clientDim.Totals, evt, costs)

		modelKey := evt.Model
		if modelKey == "" {
			modelKey = "unknown"
		}
		modelDim := byModel[modelKey]
		if modelDim == nil {
			modelDim = &DimensionTotals{Key: modelKey}
			byModel[modelKey] = modelDim
		}
		addEventTotals(&modelDim.Totals, evt, costs)
	}

	bucketKeys := make([]time.Time, 0, len(bucketMap))
	for key := range bucketMap {
		bucketKeys = append(bucketKeys, key)
	}
	sort.Slice(bucketKeys, func(i, j int) bool { return bucketKeys[i].Before(bucketKeys[j]) })
	for _, key := range bucketKeys {
		result.Buckets = append(result.Buckets, *bucketMap[key])
	}

	result.ByClient = sortedDimensions(byClient)
	result.ByModel = sortedDimensions(byModel)
	return result, nil
}

// Summary returns last hour / yesterday / last 30d totals.
func (s *Store) Summary(now time.Time, costs CostTable) (SummaryResult, error) {
	now = now.UTC()
	events, err := s.readAll()
	if err != nil {
		return SummaryResult{}, err
	}

	result := SummaryResult{GeneratedAt: now}

	// Today: from midnight UTC of current day to now.
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, evt := range events {
		t := evt.Timestamp.UTC()
		if !t.Before(todayStart) && !t.After(now) {
			addEventTotals(&result.Today, evt, costs)
		}
	}

	lastHourStart := now.Add(-1 * time.Hour)
	for _, evt := range events {
		t := evt.Timestamp.UTC()
		if !t.Before(lastHourStart) && !t.After(now) {
			addEventTotals(&result.LastHour, evt, costs)
		}
	}

	yesterdayDay := now.AddDate(0, 0, -1)
	yStart := time.Date(yesterdayDay.Year(), yesterdayDay.Month(), yesterdayDay.Day(), 0, 0, 0, 0, time.UTC)
	yEnd := yStart.Add(24 * time.Hour)
	for _, evt := range events {
		t := evt.Timestamp.UTC()
		if !t.Before(yStart) && t.Before(yEnd) {
			addEventTotals(&result.Yesterday, evt, costs)
		}
	}

	last30Start := now.AddDate(0, 0, -30)
	last7Start := now.AddDate(0, 0, -7)
	for _, evt := range events {
		t := evt.Timestamp.UTC()
		if !t.Before(last30Start) && !t.After(now) {
			addEventTotals(&result.Last30Days, evt, costs)
			if !t.Before(last7Start) {
				addEventTotals(&result.Last7Days, evt, costs)
			}
		}
	}

	return result, nil
}

func (s *Store) readAll() ([]Event, error) {
	s.mu.RLock()
	path := strings.TrimSpace(s.path)
	s.mu.RUnlock()

	if path == "" {
		return nil, errors.New("usage events file path is empty")
	}
	if err := ensureJSONLFile(path); err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open usage events file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*64)
	scanner.Buffer(buf, 1024*1024)

	items := make([]Event, 0, 128)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		evt.ClientName = strings.TrimSpace(evt.ClientName)
		evt.EndpointType = strings.ToLower(strings.TrimSpace(evt.EndpointType))
		evt.Model = strings.ToLower(strings.TrimSpace(evt.Model))
		items = append(items, evt)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan usage events file: %w", err)
	}

	return items, nil
}

func ensureJSONLFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create usage dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", filepath.Base(path), uuid.NewString()))
	if err := os.WriteFile(tmp, []byte{}, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func filterEvents(events []Event, filter Filter) []Event {
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

func normalizeGroupBy(groupBy string) string {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	if groupBy != "hour" {
		return "day"
	}
	return groupBy
}

func bucketStartFor(t time.Time, groupBy string) time.Time {
	t = t.UTC()
	if groupBy == "hour" {
		return t.Truncate(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func stepDuration(groupBy string) time.Duration {
	if groupBy == "hour" {
		return time.Hour
	}
	return 24 * time.Hour
}

func sortedDimensions(input map[string]*DimensionTotals) []DimensionTotals {
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

func addEventTotals(target *Totals, evt Event, costs CostTable) {
	target.Requests++
	target.InputTokens += evt.InputTokens
	target.OutputTokens += evt.OutputTokens
	target.CachedTokens += evt.CachedTokens
	target.EstimatedCost += estimateEventCost(evt, costs)
}

func estimateEventCost(evt Event, costs CostTable) float64 {
	rate, ok := costs.LookupCost(evt.EndpointType, evt.Model)
	if !ok {
		return 0
	}
	return float64(evt.InputTokens)/1_000_000*rate.InputPer1MTokens +
		float64(evt.OutputTokens)/1_000_000*rate.OutputPer1MTokens +
		float64(evt.CachedTokens)/1_000_000*rate.CachedInputPer1MToken
}
