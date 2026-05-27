// Package tracestore 提供 DEBUG 模式下的全量请求/响应记录。
//
// 设计文档：docs/trace-store.md
//
// 架构：
//   - 内存 Ring Buffer：sync.Map 存储最近 N 条记录，查询 O(1)
//   - 异步落盘：channel + goroutine 非阻塞写入磁盘
//   - 磁盘回退：内存 miss 时自动从磁盘文件查询（当前 + 备份）
//   - 仅 DEBUG 模式启用，生产环境零开销
package tracestore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// TraceRecord 记录单个请求的完整 META 信息和内容。
type TraceRecord struct {
	// === META: 请求侧 ===
	TraceID         string            `json:"trace_id"`
	Timestamp       time.Time         `json:"timestamp"`
	ClientName      string            `json:"client_name"`
	ClientIP        string            `json:"client_ip"`
	ClientAccessKey string            `json:"client_access_key"` // 脱敏
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	QueryParams     string            `json:"query_params"`
	RequestHeaders  map[string]string `json:"request_headers"` // 脱敏

	// === META: 路由决策 ===
	Target        string `json:"target"`
	EndpointType  string `json:"endpoint_type"`
	Model         string `json:"model"`
	SelectionKind string `json:"selection_kind"`
	KeyIndex      int    `json:"key_index"`
	KeyMask       string `json:"key_mask"`

	// === META: 上游 ===
	UpstreamURL       string            `json:"upstream_url"`
	UpstreamRequestID string            `json:"upstream_request_id"`
	UpstreamStatus    int               `json:"upstream_status"`
	UpstreamHeaders   map[string]string `json:"upstream_headers"`

	// === META: 结果 ===
	StatusCode   int   `json:"status_code"`
	DurationMS   int64 `json:"duration_ms"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CachedTokens int64 `json:"cached_tokens"`

	// === 内容 ===
	RequestBody  string `json:"request_body"`
	ResponseBody string `json:"response_body"`
}

// Config 配置 TraceStore。
type Config struct {
	Enabled        bool   `json:"enabled"`
	RingBufferSize int    `json:"ring_buffer_size"`
	MaxBodySize    int    `json:"max_body_size"`
	DiskPath       string `json:"disk_path"`
	DiskMaxSizeMB  int    `json:"disk_max_size_mb"`
	DiskMaxBackups int    `json:"disk_max_backups"`
	DiskTTLHours   int    `json:"disk_ttl_hours"`
	ChannelBuffer  int    `json:"channel_buffer"`
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		RingBufferSize: 1000,
		MaxBodySize:    512 * 1024, // 512KB
		DiskPath:       "/var/lib/llms-proxy/trace.log",
		DiskMaxSizeMB:  500, // 500MB per file
		DiskMaxBackups: 10,  // 10 files = 5GB total
		DiskTTLHours:   120, // 5 days
		ChannelBuffer:  500,
	}
}

// Store 管理 trace 记录的内存和磁盘存储。
type Store struct {
	cfg    Config
	logger *slog.Logger

	// 内存 Ring Buffer
	ring sync.Map // map[string]*TraceRecord
	keys []string // 环形队列的 key 顺序
	head int      // 环形队列头指针
	mu   sync.Mutex

	// 异步落盘
	ch   chan *TraceRecord
	done chan struct{}

	// 磁盘 writer
	diskWriter io.Writer
	diskCloser io.Closer

	// 统计
	totalRecords   atomic.Int64
	droppedRecords atomic.Int64
	diskWrites     atomic.Int64
	diskReads      atomic.Int64

	// 关闭保护
	closeOnce sync.Once
}

// New 创建 TraceStore。如果 cfg.Enabled == false，返回 noop store。
func New(cfg Config, logger *slog.Logger) *Store {
	if !cfg.Enabled {
		return &Store{cfg: cfg, logger: logger}
	}

	if logger == nil {
		logger = slog.Default()
	}

	s := &Store{
		cfg:    cfg,
		logger: logger,
		keys:   make([]string, cfg.RingBufferSize),
		ch:     make(chan *TraceRecord, cfg.ChannelBuffer),
		done:   make(chan struct{}),
	}

	// 初始化磁盘 writer
	if cfg.DiskPath != "" {
		s.initDiskWriter()
	}

	// 启动异步落盘 goroutine
	go s.diskWriterLoop()

	logger.Info("trace store enabled",
		"ring_buffer_size", cfg.RingBufferSize,
		"disk_path", cfg.DiskPath,
		"max_body_size", cfg.MaxBodySize,
	)

	return s
}

// initDiskWriter 初始化磁盘 writer（lumberjack 轮转）。
func (s *Store) initDiskWriter() {
	dir := filepath.Dir(s.cfg.DiskPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.logger.Warn("trace store: mkdir failed, disk write disabled", "path", dir, "error", err)
			return
		}
	}

	rolling := &lumberjack.Logger{
		Filename:   s.cfg.DiskPath,
		MaxSize:    s.cfg.DiskMaxSizeMB,
		MaxBackups: s.cfg.DiskMaxBackups,
		MaxAge:     max(s.cfg.DiskTTLHours/24, 1), // 至少保留 1 天，避免 <24h 时 MaxAge=0 导致不按时间轮转
		Compress:   false,                          // 不压缩，保留明文 NDJSON 方便 grep/jq 查询历史数据
	}

	s.diskWriter = rolling
	s.diskCloser = rolling
	s.logger.Info("trace store: disk writer initialized", "path", s.cfg.DiskPath)
}

// diskWriterLoop 异步落盘 goroutine。
func (s *Store) diskWriterLoop() {
	defer close(s.done)

	for record := range s.ch {
		if s.diskWriter == nil {
			continue
		}

		line, err := json.Marshal(record)
		if err != nil {
			s.logger.Warn("trace store: marshal failed", "trace_id", record.TraceID, "error", err)
			continue
		}

		line = append(line, '\n')
		if _, err := s.diskWriter.Write(line); err != nil {
			s.logger.Warn("trace store: disk write failed", "trace_id", record.TraceID, "error", err)
			continue
		}

		s.diskWrites.Add(1)
	}
}

// Record 记录一条 trace。非阻塞，channel 满时丢弃。
func (s *Store) Record(record *TraceRecord) {
	if !s.cfg.Enabled || record == nil {
		return
	}

	// 截断 body
	if s.cfg.MaxBodySize > 0 {
		if len(record.RequestBody) > s.cfg.MaxBodySize {
			record.RequestBody = record.RequestBody[:s.cfg.MaxBodySize]
		}
		if len(record.ResponseBody) > s.cfg.MaxBodySize {
			record.ResponseBody = record.ResponseBody[:s.cfg.MaxBodySize]
		}
	}

	// 写入内存 Ring Buffer
	s.ring.Store(record.TraceID, record)

	s.mu.Lock()
	// 如果环形队列已满，删除最旧的记录
	if s.keys[s.head] != "" {
		s.ring.Delete(s.keys[s.head])
	}
	s.keys[s.head] = record.TraceID
	s.head = (s.head + 1) % len(s.keys)
	s.mu.Unlock()

	s.totalRecords.Add(1)

	// 异步落盘（非阻塞）
	select {
	case s.ch <- record:
	default:
		s.droppedRecords.Add(1)
	}
}

// Get 按 trace_id 查询记录。先查内存，miss 时回退到磁盘文件。
func (s *Store) Get(traceID string) *TraceRecord {
	if !s.cfg.Enabled {
		return nil
	}

	// 先查内存
	if v, ok := s.ring.Load(traceID); ok {
		return v.(*TraceRecord)
	}

	// 内存 miss，回退到磁盘
	return s.getFromDisk(traceID)
}

// getFromDisk 从磁盘文件读取指定 trace_id 的记录。
func (s *Store) getFromDisk(traceID string) *TraceRecord {
	if s.cfg.DiskPath == "" {
		return nil
	}

	s.diskReads.Add(1)

	// 构造精确匹配模式，避免 body 内容误匹配
	matchPattern := []byte(`"trace_id":"` + traceID + `"`)

	// 读取当前文件和所有备份文件
	files := s.getTraceFiles()
	for _, file := range files {
		if record := s.searchInFile(file, traceID, matchPattern); record != nil {
			return record
		}
	}
	return nil
}

// traceFileEntry 用于排序备份文件。
type traceFileEntry struct {
	path  string
	index int // 0 = 当前文件，1 = .1 备份，2 = .2 备份...
}

// getTraceFiles 返回所有 trace 文件路径（当前文件 + 备份文件，按编号排序）。
func (s *Store) getTraceFiles() []string {
	dir := filepath.Dir(s.cfg.DiskPath)
	base := filepath.Base(s.cfg.DiskPath)

	var entries []traceFileEntry

	// 当前文件（index=0，最新）
	if _, err := os.Stat(s.cfg.DiskPath); err == nil {
		entries = append(entries, traceFileEntry{path: s.cfg.DiskPath, index: 0})
	}

	// 备份文件（trace.log.1, trace.log.2, ...）
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("trace store: failed to read trace directory", "path", dir, "error", err)
		}
		// 至少返回当前文件
		var files []string
		for _, e := range entries {
			files = append(files, e.path)
		}
		return files
	}

	for _, de := range dirEntries {
		name := de.Name()
		if name == base || !strings.HasPrefix(name, base+".") {
			continue
		}
		// 解析编号：trace.log.1 → 1, trace.log.2 → 2
		suffix := strings.TrimPrefix(name, base+".")
		idx, err := strconv.Atoi(suffix)
		if err != nil {
			continue // 跳过非数字后缀（如 .gz）
		}
		entries = append(entries, traceFileEntry{
			path:  filepath.Join(dir, name),
			index: idx,
		})
	}

	// 按编号排序：0（当前）→ 1 → 2 → ...（越新越先）
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].index < entries[j].index
	})

	files := make([]string, len(entries))
	for i, e := range entries {
		files[i] = e.path
	}
	return files
}

// searchInFile 在指定文件中搜索 trace_id。
func (s *Store) searchInFile(filePath, traceID string, matchPattern []byte) *TraceRecord {
	f, err := os.Open(filePath)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("trace store: failed to open file for search", "path", filePath, "error", err)
		}
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// 增大 buffer 以处理大行（512KB body × 2 + metadata ≈ 1.1MB）
	const maxLineSize = 2 * 1024 * 1024
	buf := make([]byte, 0, maxLineSize)
	scanner.Buffer(buf, maxLineSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		// 精确匹配 "trace_id":"xxx"，避免 body 内容误匹配
		if !bytes.Contains(line, matchPattern) {
			continue
		}

		// 完整解析
		var record TraceRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if record.TraceID == traceID {
			return &record
		}
	}

	if err := scanner.Err(); err != nil && s.logger != nil {
		s.logger.Warn("trace store: file scan error", "path", filePath, "error", err)
	}
	return nil
}

// List 列出最近 N 条记录（按时间倒序）。内存不足时回退到磁盘文件。
func (s *Store) List(limit int) []*TraceRecord {
	if !s.cfg.Enabled || limit <= 0 {
		return nil
	}

	// 第一步：在锁内收集内存记录
	s.mu.Lock()
	var memResults []*TraceRecord
	n := len(s.keys)
	start := (s.head - 1 + n) % n
	for i := 0; i < limit && i < n; i++ {
		idx := (start - i + n) % n
		key := s.keys[idx]
		if key == "" {
			break
		}
		if v, ok := s.ring.Load(key); ok {
			memResults = append(memResults, v.(*TraceRecord))
		}
	}
	s.mu.Unlock()

	// 内存记录足够，直接返回
	if len(memResults) >= limit {
		return memResults
	}

	// 第二步：释放锁后执行磁盘 I/O（不阻塞 Record）
	remaining := limit - len(memResults)
	diskRecords := s.listFromDisk(remaining)

	// 第三步：合并去重
	existing := make(map[string]bool, len(memResults))
	for _, r := range memResults {
		existing[r.TraceID] = true
	}

	results := make([]*TraceRecord, len(memResults), limit)
	copy(results, memResults)

	for _, r := range diskRecords {
		if !existing[r.TraceID] {
			results = append(results, r)
			if len(results) >= limit {
				break
			}
		}
	}

	return results
}

// listFromDisk 从磁盘文件读取最近 N 条记录。
func (s *Store) listFromDisk(limit int) []*TraceRecord {
	if s.cfg.DiskPath == "" {
		return nil
	}

	s.diskReads.Add(1)

	files := s.getTraceFiles()
	var allRecords []*TraceRecord

	// 从最新的文件开始读（当前文件最先，备份文件按编号升序）
	for _, file := range files {
		records := s.readLastNFromFile(file, limit)
		allRecords = append(allRecords, records...)
		if len(allRecords) >= limit {
			break
		}
	}

	// 按时间倒序排序（使用 sort.Slice，O(n log n)）
	sort.Slice(allRecords, func(i, j int) bool {
		return allRecords[i].Timestamp.After(allRecords[j].Timestamp)
	})

	if len(allRecords) > limit {
		allRecords = allRecords[:limit]
	}
	return allRecords
}

// readLastNFromFile 从文件末尾读取最近 N 条记录。
// 使用环形缓冲区，只保留最后 N 行，避免将整个文件读入内存。
func (s *Store) readLastNFromFile(filePath string, n int) []*TraceRecord {
	f, err := os.Open(filePath)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("trace store: failed to open file for list", "path", filePath, "error", err)
		}
		return nil
	}
	defer f.Close()

	// 环形缓冲区：只保留最后 N 行
	ring := make([]string, n)
	ringHead := 0
	ringCount := 0

	scanner := bufio.NewScanner(f)
	const maxLineSize = 2 * 1024 * 1024
	buf := make([]byte, 0, maxLineSize)
	scanner.Buffer(buf, maxLineSize)

	for scanner.Scan() {
		line := scanner.Text()
		ring[ringHead] = line
		ringHead = (ringHead + 1) % n
		if ringCount < n {
			ringCount++
		}
	}

	if err := scanner.Err(); err != nil && s.logger != nil {
		s.logger.Warn("trace store: file scan error in readLastN", "path", filePath, "error", err)
	}

	// 从环形缓冲区提取记录（按写入顺序）
	var records []*TraceRecord
	start := (ringHead - ringCount + n) % n
	for i := 0; i < ringCount; i++ {
		idx := (start + i) % n
		line := ring[idx]
		if line == "" {
			continue
		}
		var record TraceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		records = append(records, &record)
	}
	return records
}

// Stats 返回统计信息。
func (s *Store) Stats() map[string]int64 {
	return map[string]int64{
		"total_records":    s.totalRecords.Load(),
		"dropped_records":  s.droppedRecords.Load(),
		"disk_writes":      s.diskWrites.Load(),
		"disk_reads":       s.diskReads.Load(),
		"ring_buffer_size": int64(s.cfg.RingBufferSize),
	}
}

// Close 关闭 store，等待异步落盘完成。可安全多次调用。
func (s *Store) Close() error {
	if !s.cfg.Enabled {
		return nil
	}

	var err error
	s.closeOnce.Do(func() {
		close(s.ch)
		<-s.done

		if s.diskCloser != nil {
			err = s.diskCloser.Close()
		}
	})
	return err
}
