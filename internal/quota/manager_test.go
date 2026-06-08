package quota

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"

	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// --- test helpers ---

func testOpenDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := nosql.OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// putAggCell writes a test AggCell directly into the hourly agg bucket.
// key = "<2006-01-02T15>|<endpoint_type>|<client>|<model>"
func putAggCell(t *testing.T, db *bolt.DB, key string, cell nosql.AggCell) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(nosql.BucketUsageAggHourly))
		if err != nil {
			return err
		}
		data, err := json.Marshal(cell)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
	if err != nil {
		t.Fatalf("putAggCell %q: %v", key, err)
	}
}

// putClient writes a client directly to the clients bucket.
func putClient(t *testing.T, db *bolt.DB, c config.Client) {
	t.Helper()
	store := nosql.NewClientStore(db)
	if err := store.Create(c); err != nil {
		t.Fatalf("putClient %q: %v", c.Name, err)
	}
}

// putModelCost writes a model cost to the DB.
func putModelCost(t *testing.T, db *bolt.DB, mc nosql.ModelCost) {
	t.Helper()
	store := nosql.NewModelCostStore(db)
	if err := store.Upsert(mc); err != nil {
		t.Fatalf("putModelCost %v: %v", mc, err)
	}
}

// nowHourKey returns a key "YYYY-MM-DDTHH|epType|client|model" for the current hour.
func nowHourKey(epType, client, model string) string {
	hour := time.Now().UTC().Truncate(time.Hour).Format("2006-01-02T15")
	return hour + "|" + epType + "|" + client + "|" + model
}

func newTestManager(t *testing.T, db *bolt.DB) *Manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(Options{
		CostStore:   nosql.NewModelCostStore(db),
		UsageStore:  nosql.NewUsageStore(db),
		ClientStore: nosql.NewClientStore(db),
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// --- Evaluate tests ---

func TestManagerEvaluate_ClientNotFound(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// 模拟 client 之前超限，残留了 exceeded 标记
	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Dimension: "daily",
		Limit:     10.0,
		Used:      11.0,
		ResetsAt:  time.Now().UTC().Add(24 * time.Hour),
	}
	m.mu.Unlock()

	// "alice" 已被删除，Evaluate 应清除残留的 exceeded 标记
	m.Evaluate("alice")

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.exceeded["alice"]; ok {
		t.Error("expected exceeded entry for deleted client to be cleared")
	}
}

func TestManagerEvaluate_QuotaAllZero(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1"})
	m.Evaluate("alice")

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.exceeded["alice"]; ok {
		t.Error("expected no exceeded entry for client with zero quotas")
	}
}

func TestManagerEvaluate_UnderLimit(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", QuotaDailyUSD: 100})
	// Model cost: $10/1M input, $10/1M output
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	// Insert 500K input tokens + 0 output = $5
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens: 500_000,
	})

	m.Evaluate("alice")

	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.exceeded["alice"]; ok {
		t.Error("expected no exceeded for usage=$5, limit=$100")
	}
}

func TestManagerEvaluate_ExceedsLimit(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", QuotaDailyUSD: 10})
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	// 500K input + 500K output = $5 + $5 = $10 → used >= limit → exceeded
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens:  500_000,
		OutputTokens: 500_000,
	})

	m.Evaluate("alice")

	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.exceeded["alice"]
	if !ok {
		t.Fatal("expected exceeded for usage=$10, limit=$10")
	}
	if info.Dimension != DimensionDaily {
		t.Errorf("expected dimension=daily, got %q", info.Dimension)
	}
	if info.Limit != 10 {
		t.Errorf("expected limit=10, got %f", info.Limit)
	}
}

func TestManagerEvaluate_DailyAndWeeklyBothExceeded(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// Both daily=10 and weekly=10 exceeded
	putClient(t, db, config.Client{
		Name: "alice", AccessKey: "k1",
		QuotaDailyUSD: 10, QuotaWeeklyUSD: 10,
	})
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	// $10 usage
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens:  500_000,
		OutputTokens: 500_000,
	})

	m.Evaluate("alice")

	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.exceeded["alice"]
	if !ok {
		t.Fatal("expected exceeded")
	}
	// Daily should be recorded first (first in results slice order)
	if info.Dimension != DimensionDaily {
		t.Errorf("expected first-exceeded dimension=daily, got %q", info.Dimension)
	}
}

func TestManagerEvaluate_AutoClearOnCycle(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// Manually set an exceeded entry with past ResetsAt
	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Dimension: DimensionDaily,
		Limit:     10,
		Used:      15,
		ResetsAt:  time.Now().UTC().Add(-time.Hour), // in the past
	}
	m.mu.Unlock()

	// Check should auto-clear (ResetsAt in past)
	_, exceeded := m.Check("alice")
	if exceeded {
		t.Error("expected Check to auto-clear past ResetsAt")
	}
}

// --- Start/Stop tests ---

func TestManagerStart_Idempotent(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Second Start should be idempotent
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	m.Stop()
}

func TestManagerStop_Idempotent(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop()
	// Second Stop should be idempotent
	m.Stop()
}

func TestManagerStart_StopCalibration(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Calibration timer should be active.
	if m.calTimer == nil {
		t.Fatal("calTimer should be set after Start")
	}

	m.Stop()

	// After Stop, calibration timer should be stopped.
	// (calTimer.Stop() called inside Stop)
	// Verify started flag is false.
	if m.started.Load() {
		t.Fatal("started should be false after Stop")
	}
}

func TestManagerStart_PreloadClients(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	// Add a client with quota
	putClient(t, db, config.Client{Name: "alice", AccessKey: "k1", QuotaDailyUSD: 100})
	// Add some usage so Evaluate does real work
	putModelCost(t, db, nosql.ModelCost{
		EndpointType: "openai", Model: "gpt-4o",
		InputPer1MTokens: 10, OutputPer1MTokens: 10,
	})
	putAggCell(t, db, nowHourKey("openai", "alice", "gpt-4o"), nosql.AggCell{
		InputTokens: 100_000, // $1
	})

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give preload a moment to complete
	time.Sleep(100 * time.Millisecond)
	m.Stop()

	// After Start preload, alice should have been evaluated (exceeded map exists
	// but should not be set since $1 < $100)
	m.mu.RLock()
	_, exceeded := m.exceeded["alice"]
	m.mu.RUnlock()
	if exceeded {
		t.Error("expected alice NOT exceeded (usage=$1, limit=$100)")
	}
}

// --- Check tests ---

func TestManagerCheck_NoExceeded(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	_, exceeded := m.Check("alice")
	if exceeded {
		t.Error("expected not exceeded")
	}
}

func TestManagerCheck_ExceededFuture(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Dimension: DimensionDaily,
		Limit:     10,
		Used:      15,
		ResetsAt:  time.Now().UTC().Add(24 * time.Hour), // future
	}
	m.mu.Unlock()

	info, exceeded := m.Check("alice")
	if !exceeded {
		t.Fatal("expected exceeded")
	}
	if info.Dimension != DimensionDaily {
		t.Errorf("expected daily, got %q", info.Dimension)
	}
}

func TestManagerCheck_ExceededPast_AutoClear(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	m.mu.Lock()
	m.exceeded["alice"] = ExceededInfo{
		Dimension: DimensionDaily,
		Limit:     10,
		Used:      15,
		ResetsAt:  time.Now().UTC().Add(-time.Hour), // past
	}
	m.mu.Unlock()

	_, exceeded := m.Check("alice")
	if exceeded {
		t.Error("expected auto-clear for past ResetsAt")
	}

	// Verify entry was actually deleted
	m.mu.RLock()
	_, exists := m.exceeded["alice"]
	m.mu.RUnlock()
	if exists {
		t.Error("expected exceeded entry to be deleted")
	}
}

// --- Status test ---

func TestManagerStatus_QuotasInitialized(t *testing.T) {
	db := testOpenDB(t)
	m := newTestManager(t, db)

	putClient(t, db, config.Client{
		Name: "alice", AccessKey: "k1",
		QuotaDailyUSD: 10, QuotaWeeklyUSD: 50, QuotaMonthlyUSD: 200,
	})

	status := m.Status("alice")
	if status.Client != "alice" {
		t.Errorf("expected Client=alice, got %q", status.Client)
	}
	if status.Quotas == nil {
		t.Fatal("expected Quotas to be non-nil (empty map)")
	}
	// Should have daily/weekly/monthly entries
	for _, dim := range []string{DimensionDaily, DimensionWeekly, DimensionMonthly} {
		if _, ok := status.Quotas[dim]; !ok {
			t.Errorf("expected Quotas[%q] entry", dim)
		}
	}
}

func TestManagerStatus_NilStore(t *testing.T) {
	m, _ := New(Options{})
	status := m.Status("alice")
	if status.Client != "alice" {
		t.Errorf("expected Client=alice, got %q", status.Client)
	}
	if status.Quotas == nil {
		t.Error("expected Quotas to be non-nil even with nil stores")
	}
}
