package nosql

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"github.com/ycgame/llms-proxy/internal/usage"
)

// UsageStore manages usage events backed by bbolt.
// It implements the usage.Recorder interface.
//
// Two buckets are involved:
//   - usage_events     : raw event detail (RFC3339Nano + uuid key)  → for List()
//   - usage_agg_hourly : pre-aggregated hourly rollups               → for Summary/Aggregate/Count
//
// Record() writes to both in the same write tx so they stay consistent.
type UsageStore struct {
	db *bolt.DB
}

// NewUsageStore creates a new bbolt-backed usage store.
func NewUsageStore(db *bolt.DB) *UsageStore {
	return &UsageStore{db: db}
}

// Record appends one usage event AND bumps the hourly aggregation cell atomically.
func (s *UsageStore) Record(event usage.Event) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.ClientName = strings.TrimSpace(event.ClientName)
	event.EndpointType = strings.ToLower(strings.TrimSpace(event.EndpointType))
	event.Model = strings.ToLower(strings.TrimSpace(event.Model))
	event.RequestID = strings.TrimSpace(event.RequestID)
	event.Target = strings.TrimSpace(event.Target)
	event.Path = strings.TrimSpace(event.Path)

	key := usageKey(event.Timestamp, uuid.NewString())

	return s.db.Update(func(tx *bolt.Tx) error {
		// 1. Append raw detail.
		b := tx.Bucket([]byte(BucketUsageEvents))
		data, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal usage event: %w", err)
		}
		if err := b.Put([]byte(key), data); err != nil {
			return err
		}
		// 2. Bump hourly aggregation cell.
		return bumpAgg(tx, event)
	})
}

// List returns events matching the filter, sorted by timestamp descending.
//
// Optimization: scans usage_events bucket in REVERSE (Last → Prev), early-stopping
// once limit is reached. Avoids the previous full-scan + collect + sort pattern.
// For the common case (no filter or recent time window), this stops after `limit` keys.
//
// Filter semantics unchanged.
func (s *UsageStore) List(filter usage.Filter) ([]usage.Event, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}

	clientKey := strings.TrimSpace(filter.ClientName)
	modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

	events := make([]usage.Event, 0, limit)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		c := b.Cursor()

		// Compute end-bound seek key (we walk backwards from `to`).
		// Keys are "<RFC3339Nano>_<uuid>". For an inclusive upper bound on time `to`,
		// we seek to "<to+1ns>_" then step back, OR simply seek to a sentinel above all
		// keys whose timestamp ≤ to.
		var seekStart []byte
		if filter.To != nil {
			// Sentinel that sorts strictly after any "<to>_*" key but before any later timestamp.
			seekStart = []byte(filter.To.UTC().Format(time.RFC3339Nano) + "_\xff")
		}

		// Compute lower-bound prefix for early termination.
		var lowPrefix string
		if filter.From != nil {
			lowPrefix = filter.From.UTC().Format(time.RFC3339Nano)
		}

		var k, v []byte
		if seekStart != nil {
			// Seek finds first key >= seekStart, then we step back to land on the
			// largest key <= our intended bound.
			k, v = c.Seek(seekStart)
			if k == nil {
				k, v = c.Last()
			} else {
				k, v = c.Prev()
			}
		} else {
			k, v = c.Last()
		}

		for ; k != nil; k, v = c.Prev() {
			// Lower bound check: stop scanning once we go below `from`.
			if lowPrefix != "" && string(k) < lowPrefix {
				break
			}

			var evt usage.Event
			if err := json.Unmarshal(v, &evt); err != nil {
				continue
			}

			// Defensive bound checks via timestamp (handles tz / format edge cases).
			if !usageFilterMatch(evt, filter, clientKey, modelKey) {
				continue
			}

			events = append(events, evt)
			if len(events) >= limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Already in descending order due to reverse scan.
	return events, nil
}

// Count returns total and success request counts within the given time range.
//
// New implementation: scans the hourly agg bucket — bounded to ~hours-in-range cells
// times unique (et,client,model) combos per hour. For 72h window this is typically
// < 1000 cells vs. previously scanning every event in that window.
func (s *UsageStore) Count(from, to time.Time) (total, success int64, err error) {
	from = from.UTC()
	to = to.UTC()

	err = s.db.View(func(tx *bolt.Tx) error {
		return aggIter(tx, from, to, func(_ time.Time, _, _, _ string, cell AggCell) error {
			total += cell.Requests
			success += cell.Success
			return nil
		})
	})
	return
}

// Aggregate aggregates usage in requested time buckets, served from the hourly agg bucket.
//
// For group_by=hour: each agg cell maps 1:1 to a Bucket entry (further accumulated by hour).
// For group_by=day:  hour cells are accumulated into the corresponding day bucket.
// by_client / by_model totals are accumulated in the same pass.
//
// Filter semantics: ClientName / Model filters apply at the agg cell level. From/To
// are honored as inclusive hour-bucket bounds (an event whose hour falls within the
// range counts; partial-hour exclusion is approximate to ±1h, acceptable for the UI).
func (s *UsageStore) Aggregate(filter usage.Filter, groupBy string, costs usage.CostTable) (usage.AggregateResult, error) {
	groupBy = usageNormalizeGroupBy(groupBy)

	result := usage.AggregateResult{GroupBy: groupBy}
	if filter.From != nil {
		result.From = filter.From.UTC()
	}
	if filter.To != nil {
		result.To = filter.To.UTC()
	}

	from := time.Time{}
	if filter.From != nil {
		from = filter.From.UTC()
	}
	to := time.Now().UTC().Add(time.Hour)
	if filter.To != nil {
		to = filter.To.UTC()
	}

	clientKey := strings.TrimSpace(filter.ClientName)
	modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

	bucketMap := map[time.Time]*usage.Bucket{}
	byClient := map[string]*usage.DimensionTotals{}
	byModel := map[string]*usage.DimensionTotals{}

	err := s.db.View(func(tx *bolt.Tx) error {
		return aggIter(tx, from, to, func(hour time.Time, et, client, model string, cell AggCell) error {
			// Apply dimension filters.
			if clientKey != "" && client != clientKey {
				return nil
			}
			if modelKey != "" && model != modelKey {
				return nil
			}

			cost := aggCellCost(cell, et, model, costs)
			addCellToTotals(&result.Totals, cell, cost)

			bucketStart := bucketStartFromHour(hour, groupBy)
			bkt, ok := bucketMap[bucketStart]
			if !ok {
				bkt = &usage.Bucket{
					BucketStart: bucketStart,
					BucketEnd:   bucketStart.Add(usageStepDuration(groupBy)),
				}
				bucketMap[bucketStart] = bkt
			}
			addCellToTotals(&bkt.Totals, cell, cost)

			ck := client
			if ck == "" {
				ck = "unknown"
			}
			cd := byClient[ck]
			if cd == nil {
				cd = &usage.DimensionTotals{Key: ck}
				byClient[ck] = cd
			}
			addCellToTotals(&cd.Totals, cell, cost)

			mk := model
			if mk == "" {
				mk = "unknown"
			}
			md := byModel[mk]
			if md == nil {
				md = &usage.DimensionTotals{Key: mk}
				byModel[mk] = md
			}
			addCellToTotals(&md.Totals, cell, cost)
			return nil
		})
	})
	if err != nil {
		return usage.AggregateResult{}, err
	}

	bucketKeys := make([]time.Time, 0, len(bucketMap))
	for key := range bucketMap {
		bucketKeys = append(bucketKeys, key)
	}
	sort.Slice(bucketKeys, func(i, j int) bool { return bucketKeys[i].Before(bucketKeys[j]) })
	for _, key := range bucketKeys {
		result.Buckets = append(result.Buckets, *bucketMap[key])
	}

	result.ByClient = usageSortedDimensions(byClient)
	result.ByModel = usageSortedDimensions(byModel)
	return result, nil
}

// Summary returns predefined window totals for the UI.
//
// Implementation:
//   - last_hour:  scans raw usage_events for the past 1h (small volume, exact precision).
//   - today/yesterday/last_7d/last_30d: served from the hourly agg bucket.
//
// Why split: agg cells are aligned to whole hours, so a window whose start is not on
// an hour boundary (e.g. now-1h) cannot be answered exactly from agg without ±1h drift.
// last_hour is the only such window; resolving it from raw events keeps semantics exact
// while still avoiding the slow 30-day full scan that the old implementation did.
func (s *UsageStore) Summary(now time.Time, costs usage.CostTable) (usage.SummaryResult, error) {
	now = now.UTC()
	result := usage.SummaryResult{GeneratedAt: now}

	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lastHourStart := now.Add(-1 * time.Hour)
	yesterdayDay := now.AddDate(0, 0, -1)
	yStart := time.Date(yesterdayDay.Year(), yesterdayDay.Month(), yesterdayDay.Day(), 0, 0, 0, 0, time.UTC)
	yEnd := yStart.Add(24 * time.Hour)
	last7Start := now.AddDate(0, 0, -7)
	last30Start := now.AddDate(0, 0, -30)

	err := s.db.View(func(tx *bolt.Tx) error {
		// --- last_hour: precise scan over raw events ---
		eventsBucket := tx.Bucket([]byte(BucketUsageEvents))
		if eventsBucket != nil {
			c := eventsBucket.Cursor()
			seekKey := []byte(lastHourStart.Format(time.RFC3339Nano))
			for k, v := c.Seek(seekKey); k != nil; k, v = c.Next() {
				var evt usage.Event
				if err := json.Unmarshal(v, &evt); err != nil {
					continue
				}
				t := evt.Timestamp.UTC()
				if t.After(now) || t.Before(lastHourStart) {
					continue
				}
				cost := usageEstimateEventCost(evt, costs)
				addEventToTotals(&result.LastHour, evt, cost)
			}
		}

		// --- other windows: from hourly agg bucket ---
		return aggIter(tx, last30Start, now, func(hour time.Time, et, _, model string, cell AggCell) error {
			cost := aggCellCost(cell, et, model, costs)

			if !hour.Before(last30Start) && !hour.After(now) {
				addCellToTotals(&result.Last30Days, cell, cost)
				if !hour.Before(last7Start) {
					addCellToTotals(&result.Last7Days, cell, cost)
				}
				if !hour.Before(todayStart) {
					addCellToTotals(&result.Today, cell, cost)
				}
				if !hour.Before(yStart) && hour.Before(yEnd) {
					addCellToTotals(&result.Yesterday, cell, cost)
				}
			}
			return nil
		})
	})
	if err != nil {
		return usage.SummaryResult{}, err
	}

	return result, nil
}

// addEventToTotals adds one raw event into a Totals accumulator (used by last_hour scan).
func addEventToTotals(target *usage.Totals, evt usage.Event, cost float64) {
	target.Requests++
	target.InputTokens += evt.InputTokens
	target.OutputTokens += evt.OutputTokens
	target.CachedTokens += evt.CachedTokens
	target.CacheCreationTokens += evt.CacheCreationTokens
	target.EstimatedCost += cost
}

// usageEstimateEventCost computes cost for a single raw event.
func usageEstimateEventCost(evt usage.Event, costs usage.CostTable) float64 {
	if costs == nil {
		return 0
	}
	rate, ok := costs.LookupCost(evt.Model)
	if !ok {
		return 0
	}
	return float64(evt.InputTokens)/1_000_000*rate.InputPer1MTokens +
		float64(evt.OutputTokens)/1_000_000*rate.OutputPer1MTokens +
		float64(evt.CacheCreationTokens)/1_000_000*rate.CachedInputPer1MToken +
		float64(evt.CachedTokens)/1_000_000*rate.CacheReadPer1MToken
}

// --- Internal helpers ---

func usageKey(t time.Time, id string) string {
	return t.UTC().Format(time.RFC3339Nano) + "_" + id
}

func usageFilterMatch(evt usage.Event, filter usage.Filter, clientKey, modelKey string) bool {
	t := evt.Timestamp.UTC()
	if filter.From != nil && t.Before(filter.From.UTC()) {
		return false
	}
	if filter.To != nil && t.After(filter.To.UTC()) {
		return false
	}
	if clientKey != "" && evt.ClientName != clientKey {
		return false
	}
	if modelKey != "" && strings.ToLower(evt.Model) != modelKey {
		return false
	}
	return true
}

func usageNormalizeGroupBy(groupBy string) string {
	groupBy = strings.ToLower(strings.TrimSpace(groupBy))
	if groupBy != "hour" {
		return "day"
	}
	return groupBy
}

func usageStepDuration(groupBy string) time.Duration {
	if groupBy == "hour" {
		return time.Hour
	}
	return 24 * time.Hour
}

// bucketStartFromHour rounds an hour-bucket timestamp down to the requested granularity.
func bucketStartFromHour(hour time.Time, groupBy string) time.Time {
	hour = hour.UTC()
	if groupBy == "hour" {
		return hour
	}
	return time.Date(hour.Year(), hour.Month(), hour.Day(), 0, 0, 0, 0, time.UTC)
}

// addCellToTotals adds one agg cell into a Totals accumulator.
func addCellToTotals(target *usage.Totals, cell AggCell, cost float64) {
	target.Requests += cell.Requests
	target.InputTokens += cell.InputTokens
	target.OutputTokens += cell.OutputTokens
	target.CachedTokens += cell.CachedTokens
	target.CacheCreationTokens += cell.CacheCreationTokens
	target.EstimatedCost += cost
}

// aggCellCost computes estimated cost for one agg cell given its model.
func aggCellCost(cell AggCell, endpointType, model string, costs usage.CostTable) float64 {
	if costs == nil {
		return 0
	}
	rate, ok := costs.LookupCost(model)
	if !ok {
		return 0
	}
	return float64(cell.InputTokens)/1_000_000*rate.InputPer1MTokens +
		float64(cell.OutputTokens)/1_000_000*rate.OutputPer1MTokens +
		float64(cell.CacheCreationTokens)/1_000_000*rate.CachedInputPer1MToken +
		float64(cell.CachedTokens)/1_000_000*rate.CacheReadPer1MToken
}

func usageSortedDimensions(input map[string]*usage.DimensionTotals) []usage.DimensionTotals {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]usage.DimensionTotals, 0, len(keys))
	for _, key := range keys {
		result = append(result, *input[key])
	}
	return result
}
