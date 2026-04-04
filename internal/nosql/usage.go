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
type UsageStore struct {
	db *bolt.DB
}

// NewUsageStore creates a new bbolt-backed usage store.
func NewUsageStore(db *bolt.DB) *UsageStore {
	return &UsageStore{db: db}
}

// Record appends one usage event (implements usage.Recorder).
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
		b := tx.Bucket([]byte(BucketUsageEvents))
		data, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal usage event: %w", err)
		}
		return b.Put([]byte(key), data)
	})
}

// List returns events matching the filter, sorted by timestamp descending.
func (s *UsageStore) List(filter usage.Filter) ([]usage.Event, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}

	var events []usage.Event
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		c := b.Cursor()

		// Determine seek start point.
		var startKey []byte
		if filter.From != nil {
			startKey = []byte(filter.From.UTC().Format(time.RFC3339Nano))
		}

		// Determine end boundary.
		var endPrefix string
		if filter.To != nil {
			endPrefix = filter.To.UTC().Format(time.RFC3339Nano)
		}

		clientKey := strings.TrimSpace(filter.ClientName)
		modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

		var k, v []byte
		if startKey != nil {
			k, v = c.Seek(startKey)
		} else {
			k, v = c.First()
		}

		for ; k != nil; k, v = c.Next() {
			// If we have an end boundary, stop when key exceeds it.
			if endPrefix != "" && string(k) > endPrefix+"_\xff" {
				break
			}

			var evt usage.Event
			if err := json.Unmarshal(v, &evt); err != nil {
				continue
			}

			if !usageFilterMatch(evt, filter, clientKey, modelKey) {
				continue
			}
			events = append(events, evt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort descending by timestamp.
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.After(events[j].Timestamp)
	})

	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// Aggregate aggregates usage in requested time buckets.
func (s *UsageStore) Aggregate(filter usage.Filter, groupBy string, costs usage.CostTable) (usage.AggregateResult, error) {
	groupBy = usageNormalizeGroupBy(groupBy)

	result := usage.AggregateResult{GroupBy: groupBy}
	if filter.From != nil {
		result.From = filter.From.UTC()
	}
	if filter.To != nil {
		result.To = filter.To.UTC()
	}

	bucketMap := map[time.Time]*usage.Bucket{}
	byClient := map[string]*usage.DimensionTotals{}
	byModel := map[string]*usage.DimensionTotals{}

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		c := b.Cursor()

		var startKey []byte
		if filter.From != nil {
			startKey = []byte(filter.From.UTC().Format(time.RFC3339Nano))
		}

		var endPrefix string
		if filter.To != nil {
			endPrefix = filter.To.UTC().Format(time.RFC3339Nano)
		}

		clientKey := strings.TrimSpace(filter.ClientName)
		modelKey := strings.ToLower(strings.TrimSpace(filter.Model))

		var k, v []byte
		if startKey != nil {
			k, v = c.Seek(startKey)
		} else {
			k, v = c.First()
		}

		for ; k != nil; k, v = c.Next() {
			if endPrefix != "" && string(k) > endPrefix+"_\xff" {
				break
			}

			var evt usage.Event
			if err := json.Unmarshal(v, &evt); err != nil {
				continue
			}

			if !usageFilterMatch(evt, filter, clientKey, modelKey) {
				continue
			}

			usageAddEventTotals(&result.Totals, evt, costs)

			bucketStart := usageBucketStartFor(evt.Timestamp.UTC(), groupBy)
			bkt, ok := bucketMap[bucketStart]
			if !ok {
				bkt = &usage.Bucket{
					BucketStart: bucketStart,
					BucketEnd:   usageBucketStartFor(bucketStart.Add(usageStepDuration(groupBy)), groupBy),
				}
				bucketMap[bucketStart] = bkt
			}
			usageAddEventTotals(&bkt.Totals, evt, costs)

			ck := evt.ClientName
			if ck == "" {
				ck = "unknown"
			}
			clientDim := byClient[ck]
			if clientDim == nil {
				clientDim = &usage.DimensionTotals{Key: ck}
				byClient[ck] = clientDim
			}
			usageAddEventTotals(&clientDim.Totals, evt, costs)

			mk := evt.Model
			if mk == "" {
				mk = "unknown"
			}
			modelDim := byModel[mk]
			if modelDim == nil {
				modelDim = &usage.DimensionTotals{Key: mk}
				byModel[mk] = modelDim
			}
			usageAddEventTotals(&modelDim.Totals, evt, costs)
		}
		return nil
	})
	if err != nil {
		return usage.AggregateResult{}, err
	}

	// Sort buckets by time.
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
		b := tx.Bucket([]byte(BucketUsageEvents))
		if b == nil {
			return nil
		}
		c := b.Cursor()

		// Seek to last30Start to minimize scanning.
		seekKey := []byte(last30Start.Format(time.RFC3339Nano))
		for k, v := c.Seek(seekKey); k != nil; k, v = c.Next() {
			var evt usage.Event
			if err := json.Unmarshal(v, &evt); err != nil {
				continue
			}

			t := evt.Timestamp.UTC()
			if t.After(now) {
				continue
			}

			// Last 30 days.
			if !t.Before(last30Start) {
				usageAddEventTotals(&result.Last30Days, evt, costs)

				// Last 7 days.
				if !t.Before(last7Start) {
					usageAddEventTotals(&result.Last7Days, evt, costs)
				}

				// Today.
				if !t.Before(todayStart) {
					usageAddEventTotals(&result.Today, evt, costs)
				}

				// Last hour.
				if !t.Before(lastHourStart) {
					usageAddEventTotals(&result.LastHour, evt, costs)
				}

				// Yesterday.
				if !t.Before(yStart) && t.Before(yEnd) {
					usageAddEventTotals(&result.Yesterday, evt, costs)
				}
			}
		}
		return nil
	})
	if err != nil {
		return usage.SummaryResult{}, err
	}

	return result, nil
}

// --- Internal helper functions (reimplemented from usage package) ---

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

func usageBucketStartFor(t time.Time, groupBy string) time.Time {
	t = t.UTC()
	if groupBy == "hour" {
		return t.Truncate(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func usageStepDuration(groupBy string) time.Duration {
	if groupBy == "hour" {
		return time.Hour
	}
	return 24 * time.Hour
}

func usageAddEventTotals(target *usage.Totals, evt usage.Event, costs usage.CostTable) {
	target.Requests++
	target.InputTokens += evt.InputTokens
	target.OutputTokens += evt.OutputTokens
	target.CachedTokens += evt.CachedTokens
	target.EstimatedCost += usageEstimateEventCost(evt, costs)
}

func usageEstimateEventCost(evt usage.Event, costs usage.CostTable) float64 {
	rate, ok := costs.LookupCost(evt.EndpointType, evt.Model)
	if !ok {
		return 0
	}
	return float64(evt.InputTokens)/1_000_000*rate.InputPer1MTokens +
		float64(evt.OutputTokens)/1_000_000*rate.OutputPer1MTokens +
		float64(evt.CachedTokens)/1_000_000*rate.CachedInputPer1MToken
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
