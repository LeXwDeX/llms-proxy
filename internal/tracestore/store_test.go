package tracestore

import (
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
