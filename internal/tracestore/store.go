// Package tracestore 提供 DEBUG 模式下的全量请求/响应记录。
//
// 设计文档：docs/trace-store.md
//
// 架构：
//   - 内存 Ring Buffer：sync.Map 存储最近 N 条记录，查询 O(1)
//   - 异步落盘：channel + goroutine 非阻塞写入磁盘
//   - 仅 DEBUG 模式启用，生产环境零开销
package tracestore

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	DiskTTLHours   int    `json:"disk_ttl_hours"`
	ChannelBuffer  int    `json:"channel_buffer"`
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		RingBufferSize: 1000,
		MaxBodySize:    2 * 1024 * 1024, // 2MB
		DiskPath:       "/var/lib/llms-proxy/trace.log",
		DiskMaxSizeMB:  1024, // 1GB
		DiskTTLHours:   24,
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
		MaxBackups: 3,
		MaxAge:     s.cfg.DiskTTLHours / 24,
		Compress:   true,
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

// Get 按 trace_id 查询记录。先查内存，miss 返回 nil。
func (s *Store) Get(traceID string) *TraceRecord {
	if !s.cfg.Enabled {
		return nil
	}

	if v, ok := s.ring.Load(traceID); ok {
		return v.(*TraceRecord)
	}
	return nil
}

// List 列出最近 N 条记录（按时间倒序）。
func (s *Store) List(limit int) []*TraceRecord {
	if !s.cfg.Enabled || limit <= 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var results []*TraceRecord
	n := len(s.keys)
	start := (s.head - 1 + n) % n

	for i := 0; i < limit && i < n; i++ {
		idx := (start - i + n) % n
		key := s.keys[idx]
		if key == "" {
			break
		}
		if v, ok := s.ring.Load(key); ok {
			results = append(results, v.(*TraceRecord))
		}
	}

	return results
}

// Stats 返回统计信息。
func (s *Store) Stats() map[string]int64 {
	return map[string]int64{
		"total_records":   s.totalRecords.Load(),
		"dropped_records": s.droppedRecords.Load(),
		"disk_writes":     s.diskWrites.Load(),
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
