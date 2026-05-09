package nosql

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ycgame/llms-proxy/internal/usage"
)

// 预聚合层（hourly bucket）
//
// 设计：
//   - key  = "<2006-01-02T15>|<endpoint_type>|<client>|<model>"
//     timestamp 用截断到小时的字典序友好格式（UTC）；分隔符 '|' 不会出现在小时格式串中。
//   - val  = JSON{requests, success, input_tokens, output_tokens, cached_tokens}
//   - 写：Record() 在同一 bbolt 写事务内 read-modify-write 累加。
//   - 读：Summary/Aggregate/Count 仅扫此 bucket，避免百万级明细扫描 + Unmarshal。
//
// 维度选择不含 status_code：只 healthz 关心 success/total 比例，单独存 success 字段足矣，
// 避免 status 维度炸开 cardinality。
//
// 不做 daily bucket：日聚合可由小时桶累加得出，避免写放大。

const (
	// BucketUsageAggHourly stores hourly pre-aggregated usage rollups.
	BucketUsageAggHourly = "usage_agg_hourly"
	// usageAggBuiltMetaKey marks one-shot backfill completion (v1 schema).
	usageAggBuiltMetaKey = "usage_agg_built_at_v1"
	// hourLayout is the bucket-truncated hour key in UTC.
	hourLayout = "2006-01-02T15"
)

// AggCell is the persisted value of one hourly aggregation cell.
type AggCell struct {
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
}

// aggKey builds the composite key for an aggregation cell.
func aggKey(t time.Time, endpointType, client, model string) []byte {
	hour := t.UTC().Truncate(time.Hour).Format(hourLayout)
	et := strings.ToLower(strings.TrimSpace(endpointType))
	c := strings.TrimSpace(client)
	m := strings.ToLower(strings.TrimSpace(model))
	if c == "" {
		c = "unknown"
	}
	if m == "" {
		m = "unknown"
	}
	// Note: et may be empty (legacy events without endpoint_type). Keep as empty segment.
	return []byte(hour + "|" + et + "|" + c + "|" + m)
}

// parseAggKey decomposes a key back into (hour, endpoint_type, client, model).
// Returns ok=false if the key is malformed.
func parseAggKey(key []byte) (hour time.Time, endpointType, client, model string, ok bool) {
	parts := strings.SplitN(string(key), "|", 4)
	if len(parts) != 4 {
		return time.Time{}, "", "", "", false
	}
	t, err := time.ParseInLocation(hourLayout, parts[0], time.UTC)
	if err != nil {
		return time.Time{}, "", "", "", false
	}
	return t, parts[1], parts[2], parts[3], true
}

// hourPrefix returns the hour-bucket-truncated key prefix for time t (used as Seek bound).
func hourPrefix(t time.Time) []byte {
	return []byte(t.UTC().Truncate(time.Hour).Format(hourLayout))
}

// isSuccessStatus mirrors the existing Count() definition: 0 < code < 500.
// StatusCode == 0 (legacy/missing) is treated as NOT success (conservative).
func isSuccessStatus(code int) bool {
	return code > 0 && code < 500
}

// bumpAgg performs read-modify-write accumulation for one event in the agg bucket.
// MUST be called inside a write transaction.
func bumpAgg(tx *bolt.Tx, evt usage.Event) error {
	b := tx.Bucket([]byte(BucketUsageAggHourly))
	if b == nil {
		return fmt.Errorf("agg bucket missing")
	}
	key := aggKey(evt.Timestamp, evt.EndpointType, evt.ClientName, evt.Model)

	var cell AggCell
	if existing := b.Get(key); existing != nil {
		if err := json.Unmarshal(existing, &cell); err != nil {
			// Corrupt cell: reset rather than abort (defensive).
			cell = AggCell{}
		}
	}
	cell.Requests++
	if isSuccessStatus(evt.StatusCode) {
		cell.Success++
	}
	cell.InputTokens += evt.InputTokens
	cell.OutputTokens += evt.OutputTokens
	cell.CachedTokens += evt.CachedTokens

	data, err := json.Marshal(cell)
	if err != nil {
		return fmt.Errorf("marshal agg cell: %w", err)
	}
	return b.Put(key, data)
}

// aggIter visits all cells whose hour bucket falls within [from, to] inclusive.
// fn receives the parsed dimensions and the cell value. Returning an error stops iteration.
func aggIter(tx *bolt.Tx, from, to time.Time, fn func(hour time.Time, endpointType, client, model string, cell AggCell) error) error {
	b := tx.Bucket([]byte(BucketUsageAggHourly))
	if b == nil {
		return nil
	}
	c := b.Cursor()

	startKey := hourPrefix(from)
	// endPrefix is the inclusive last hour; we stop when key > endPrefix + sentinel.
	// "|" (0x7C) sorts before "}" (0x7D), so endPrefix+"}" is a clean upper bound for
	// any key starting with endPrefix + "|...".
	endStop := append(hourPrefix(to), '}')

	for k, v := c.Seek(startKey); k != nil; k, v = c.Next() {
		if string(k) > string(endStop) {
			break
		}
		hour, et, client, model, ok := parseAggKey(k)
		if !ok {
			continue
		}
		var cell AggCell
		if err := json.Unmarshal(v, &cell); err != nil {
			continue
		}
		if err := fn(hour, et, client, model, cell); err != nil {
			return err
		}
	}
	return nil
}

// BackfillUsageAgg scans all existing usage_events and rebuilds the hourly agg bucket.
// Idempotent: skips if meta marker exists. Safe to run on startup.
//
// Concurrency: bbolt serializes write txs, so concurrent Record() during backfill
// is safe — both paths converge on the same key via read-modify-write accumulation.
// However, to avoid double-counting (an event written AFTER backfill marks the cell
// but BEFORE we commit), backfill happens BEFORE the proxy starts accepting traffic.
func BackfillUsageAgg(db *bolt.DB) error {
	// Already done?
	var done bool
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		if b == nil {
			return nil
		}
		if b.Get([]byte(usageAggBuiltMetaKey)) != nil {
			done = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("check agg meta: %w", err)
	}
	if done {
		return nil
	}

	// Stream events and rebuild in batches to keep each write tx bounded.
	// Pattern: read a chunk in a View tx, commit it via an Update tx, advance lastKey.
	const batchSize = 5000
	var (
		batch     []usage.Event
		processed int64
		lastKey   []byte
	)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := db.Update(func(tx *bolt.Tx) error {
			for _, evt := range batch {
				if err := bumpAgg(tx, evt); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		processed += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for {
		batch = batch[:0]
		err := db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketUsageEvents))
			if b == nil {
				return nil
			}
			c := b.Cursor()
			var k, v []byte
			if lastKey == nil {
				k, v = c.First()
			} else {
				// Resume strictly after lastKey.
				k, v = c.Seek(lastKey)
				if k != nil && string(k) == string(lastKey) {
					k, v = c.Next()
				}
			}
			for ; k != nil && len(batch) < batchSize; k, v = c.Next() {
				var evt usage.Event
				if err := json.Unmarshal(v, &evt); err != nil {
					// Still advance lastKey so we don't loop forever on a bad row.
					lastKey = append(lastKey[:0], k...)
					continue
				}
				batch = append(batch, evt)
				lastKey = append(lastKey[:0], k...)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan usage_events chunk: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		if err := flush(); err != nil {
			return fmt.Errorf("flush agg batch: %w", err)
		}
	}

	// Mark done.
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketMeta))
		marker, _ := json.Marshal(map[string]any{
			"built_at":  time.Now().UTC().Format(time.RFC3339),
			"processed": processed,
		})
		return b.Put([]byte(usageAggBuiltMetaKey), marker)
	})
	if err != nil {
		return fmt.Errorf("write agg meta: %w", err)
	}

	return nil
}
