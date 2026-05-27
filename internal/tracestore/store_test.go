package tracestore

import (
	"os"
	"path/filepath"
	"strings"
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
	largeBody := strings.Repeat("x", 100)

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
	if cfg.MaxBodySize != 512*1024 {
		t.Errorf("expected MaxBodySize 512KB, got %d", cfg.MaxBodySize)
	}
	if cfg.DiskPath != "/var/lib/llms-proxy/trace.log" {
		t.Errorf("expected default DiskPath, got %s", cfg.DiskPath)
	}
	if cfg.DiskMaxSizeMB != 500 {
		t.Errorf("expected DiskMaxSizeMB 500, got %d", cfg.DiskMaxSizeMB)
	}
	if cfg.DiskMaxBackups != 10 {
		t.Errorf("expected DiskMaxBackups 10, got %d", cfg.DiskMaxBackups)
	}
	if cfg.DiskTTLHours != 120 {
		t.Errorf("expected DiskTTLHours 120 (5 days), got %d", cfg.DiskTTLHours)
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

func TestStoreGetFromDisk(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-disk-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 2, // 很小的 ring buffer，会很快溢出
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// 写入 5 条记录（ring buffer 只能存 2 条，其余只在磁盘）
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "disk-test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
			Model:     "test-model",
		})
	}

	// 等待异步落盘
	store.Close()

	// 重新创建 store（模拟重启），ring buffer 为空
	store2 := New(cfg, nil)
	defer store2.Close()

	// 内存中应该没有记录
	if _, ok := store2.ring.Load("disk-test-0"); ok {
		t.Error("expected memory to be empty after restart")
	}

	// 但 Get 应该能从磁盘读取
	record := store2.Get("disk-test-0")
	if record == nil {
		t.Fatal("expected Get to find record from disk")
	}
	if record.TraceID != "disk-test-0" {
		t.Errorf("expected trace_id disk-test-0, got %s", record.TraceID)
	}

	// 查询最后一条记录
	record = store2.Get("disk-test-4")
	if record == nil {
		t.Fatal("expected Get to find last record from disk")
	}
}

func TestStoreListFromDisk(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-list-disk-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 3, // 小 ring buffer
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// 写入 10 条记录
	for i := 0; i < 10; i++ {
		store.Record(&TraceRecord{
			TraceID:   "list-test-" + string(rune('0'+i)),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Model:     "test-model",
		})
	}

	// 等待异步落盘
	store.Close()

	// 重新创建 store（模拟重启）
	store2 := New(cfg, nil)
	defer store2.Close()

	// List 应该能从磁盘读取记录
	records := store2.List(5)
	if len(records) != 5 {
		t.Errorf("expected 5 records from List, got %d", len(records))
	}

	// 验证记录按时间倒序
	for i := 1; i < len(records); i++ {
		if records[i].Timestamp.After(records[i-1].Timestamp) {
			t.Errorf("records not sorted by time: %v > %v", records[i].Timestamp, records[i-1].Timestamp)
		}
	}

	// List 更多记录
	records = store2.List(10)
	if len(records) != 10 {
		t.Errorf("expected 10 records from List, got %d", len(records))
	}
}

func TestStoreGetFromDiskWithBackups(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-backup-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	// 手动创建当前文件和备份文件
	currentContent := `{"trace_id":"current-1","timestamp":"2026-05-27T10:00:00Z","model":"test"}
{"trace_id":"current-2","timestamp":"2026-05-27T10:01:00Z","model":"test"}
`
	backup1Content := `{"trace_id":"backup1-1","timestamp":"2026-05-27T09:00:00Z","model":"test"}
{"trace_id":"backup1-2","timestamp":"2026-05-27T09:01:00Z","model":"test"}
`

	if err := os.WriteFile(diskPath, []byte(currentContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath+".1", []byte(backup1Content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// 查询当前文件中的记录
	record := store.Get("current-1")
	if record == nil {
		t.Fatal("expected to find current-1 from current file")
	}

	// 查询备份文件中的记录
	record = store.Get("backup1-1")
	if record == nil {
		t.Fatal("expected to find backup1-1 from backup file")
	}

	// 查询不存在的记录
	record = store.Get("nonexistent")
	if record != nil {
		t.Error("expected nil for nonexistent record")
	}
}
