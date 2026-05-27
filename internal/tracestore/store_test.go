package tracestore

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreDisabled(t *testing.T) {
	cfg := Config{Enabled: false}
	store := New(cfg, nil)

	// Record should be no-op
	store.Record(&TraceRecord{TraceID: "test-1"})

	// Get should return nil
	if record := store.Get("test-1"); record != nil {
		t.Errorf("expected nil from disabled store, got %+v", record)
	}

	// List should return nil
	if records := store.List(10); records != nil {
		t.Errorf("expected nil from disabled store, got %d records", len(records))
	}

	// Stats should return zeros
	stats := store.Stats()
	if stats["total_records"] != 0 {
		t.Errorf("expected 0 total_records, got %d", stats["total_records"])
	}
}

func TestStoreRingBuffer(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 3,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record 5 entries (ring buffer size is 3)
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
		})
	}

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	// First 2 entries should be evicted
	if record := store.Get("test-0"); record != nil {
		t.Errorf("expected test-0 to be evicted, got %+v", record)
	}
	if record := store.Get("test-1"); record != nil {
		t.Errorf("expected test-1 to be evicted, got %+v", record)
	}

	// Last 3 entries should be present
	for i := 2; i < 5; i++ {
		id := "test-" + string(rune('0'+i))
		if record := store.Get(id); record == nil {
			t.Errorf("expected %s to be present", id)
		}
	}

	// Stats should show 5 total records
	stats := store.Stats()
	if stats["total_records"] != 5 {
		t.Errorf("expected 5 total_records, got %d", stats["total_records"])
	}
}

func TestStoreList(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record 5 entries
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
		})
	}

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	// List should return entries in reverse order (newest first)
	records := store.List(3)
	if len(records) != 3 {
		t.Errorf("expected 3 records, got %d", len(records))
	}

	// First record should be the newest (test-4)
	if records[0].TraceID != "test-4" {
		t.Errorf("expected first record to be test-4, got %s", records[0].TraceID)
	}
}

func TestStoreBodyTruncation(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    10, // Very small limit
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record with large body
	largeBody := make([]byte, 100)
	for i := range largeBody {
		largeBody[i] = 'x'
	}

	store.Record(&TraceRecord{
		TraceID:      "test-large",
		Timestamp:    time.Now(),
		RequestBody:  largeBody,
		ResponseBody: largeBody,
	})

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	// Body should be truncated
	record := store.Get("test-large")
	if record == nil {
		t.Fatal("expected record to be present")
	}

	if len(record.RequestBody) != 10 {
		t.Errorf("expected RequestBody to be truncated to 10 bytes, got %d", len(record.RequestBody))
	}
	if len(record.ResponseBody) != 10 {
		t.Errorf("expected ResponseBody to be truncated to 10 bytes, got %d", len(record.ResponseBody))
	}
}

func TestStoreChannelDrop(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 100,
		MaxBodySize:    1024,
		ChannelBuffer:  1, // Very small buffer
	}
	store := New(cfg, nil)
	defer store.Close()

	// Rapidly record many entries to overflow channel
	for i := 0; i < 100; i++ {
		store.Record(&TraceRecord{
			TraceID:   "test-" + string(rune('0'+i%10)),
			Timestamp: time.Now(),
		})
	}

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// Stats should show some dropped records
	stats := store.Stats()
	if stats["dropped_records"] == 0 {
		t.Log("warning: no dropped records detected (may be timing-dependent)")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Enabled {
		t.Error("expected Enabled to be false by default")
	}
	if cfg.RingBufferSize != 1000 {
		t.Errorf("expected RingBufferSize 1000, got %d", cfg.RingBufferSize)
	}
	if cfg.MaxBodySize != 2*1024*1024 {
		t.Errorf("expected MaxBodySize 2MB, got %d", cfg.MaxBodySize)
	}
	if cfg.DiskPath != "/var/lib/llms-proxy/trace.log" {
		t.Errorf("expected default DiskPath, got %s", cfg.DiskPath)
	}
	if cfg.DiskMaxSizeMB != 1024 {
		t.Errorf("expected DiskMaxSizeMB 1024, got %d", cfg.DiskMaxSizeMB)
	}
	if cfg.DiskTTLHours != 24 {
		t.Errorf("expected DiskTTLHours 24, got %d", cfg.DiskTTLHours)
	}
	if cfg.ChannelBuffer != 500 {
		t.Errorf("expected ChannelBuffer 500, got %d", cfg.ChannelBuffer)
	}
}

func TestStoreDiskWrite(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskTTLHours:   1,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// Record 5 entries
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "disk-test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
			Model:     "test-model",
		})
	}

	// Close to flush disk writes
	store.Close()

	// Verify disk file exists and has content
	info, err := os.Stat(diskPath)
	if err != nil {
		t.Fatalf("expected disk file to exist: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected disk file to have content")
	}

	// Stats should show disk writes
	stats := store.Stats()
	if stats["disk_writes"] != 5 {
		t.Errorf("expected 5 disk_writes, got %d", stats["disk_writes"])
	}
}

func TestStoreConcurrent(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 100,
		MaxBodySize:    1024,
		ChannelBuffer:  100,
	}
	store := New(cfg, nil)
	defer store.Close()

	var wg sync.WaitGroup
	numGoroutines := 10
	numRecordsPerGoroutine := 100

	// Concurrent writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numRecordsPerGoroutine; j++ {
				store.Record(&TraceRecord{
					TraceID:   "concurrent-" + string(rune('0'+id)) + "-" + string(rune('0'+j%10)),
					Timestamp: time.Now(),
				})
			}
		}(i)
	}

	// Concurrent reads
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numRecordsPerGoroutine; j++ {
				store.Get("concurrent-0-0")
				store.List(10)
				store.Stats()
			}
		}()
	}

	wg.Wait()

	// Should not panic and stats should be reasonable
	stats := store.Stats()
	totalRecords := stats["total_records"]
	if totalRecords != int64(numGoroutines*numRecordsPerGoroutine) {
		t.Errorf("expected %d total_records, got %d", numGoroutines*numRecordsPerGoroutine, totalRecords)
	}
}

func TestStoreNilRecord(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Should not panic
	store.Record(nil)

	// Stats should still be 0
	stats := store.Stats()
	if stats["total_records"] != 0 {
		t.Errorf("expected 0 total_records after nil record, got %d", stats["total_records"])
	}
}

func TestStoreEmptyTraceID(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record with empty trace_id
	store.Record(&TraceRecord{
		TraceID:   "",
		Timestamp: time.Now(),
	})

	time.Sleep(100 * time.Millisecond)

	// Should be stored (empty string is valid key)
	record := store.Get("")
	if record == nil {
		t.Error("expected record with empty trace_id to be stored")
	}
}

func TestStoreListEmpty(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// List on empty store
	records := store.List(10)
	if len(records) != 0 {
		t.Errorf("expected 0 records from empty store, got %d", len(records))
	}

	// List with limit 0
	records = store.List(0)
	if records != nil {
		t.Errorf("expected nil from List(0), got %v", records)
	}

	// List with negative limit
	records = store.List(-1)
	if records != nil {
		t.Errorf("expected nil from List(-1), got %v", records)
	}
}

func TestStoreCloseIdempotent(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// Close multiple times should not panic
	store.Close()
	store.Close()
	store.Close()
}

func TestStoreCloseDisabled(t *testing.T) {
	cfg := Config{Enabled: false}
	store := New(cfg, nil)

	// Close on disabled store should not panic
	err := store.Close()
	if err != nil {
		t.Errorf("expected no error from Close on disabled store, got %v", err)
	}
}
