# Trace Store — DEBUG 模式全量请求/响应记录

## 目标

DEBUG 模式下记录每个请求的完整 META 信息（上下游元数据）和内容（请求/响应 body），支持按 request_id 快速查询。

## 设计原则

1. **仅 DEBUG 模式**：生产环境零开销（一次 bool 检查）
2. **内存优先**：Ring Buffer 存最近 N 条，查询 O(1)
3. **异步落盘**：goroutine 非阻塞写入磁盘，宕机可丢失
4. **完整记录**：META（上下游元数据）+ 内容（请求/响应 body）
5. **独立存储**：与 bbolt、日志文件完全隔离

## 数据结构

```go
type TraceRecord struct {
    // === META: 请求侧 ===
    TraceID         string    `json:"trace_id"`          // X-Request-ID
    Timestamp       time.Time `json:"timestamp"`         // 请求到达时间
    ClientName      string    `json:"client_name"`       // 客户端名称
    ClientIP        string    `json:"client_ip"`         // 客户端 IP
    ClientAccessKey string    `json:"client_access_key"` // 客户端 access_key（脱敏）
    Method          string    `json:"method"`            // HTTP method
    Path            string    `json:"path"`              // 请求路径
    QueryParams     string    `json:"query_params"`      // URL query string
    RequestHeaders  map[string]string `json:"request_headers"` // 请求头（脱敏）

    // === META: 路由决策 ===
    Target          string    `json:"target"`            // 选中的 target name
    EndpointType    string    `json:"endpoint_type"`     // endpoint type
    Model           string    `json:"model"`             // 请求的模型
    SelectionKind   string    `json:"selection_kind"`    // explicit/affinity/roundRobin/failover
    KeyIndex        int       `json:"key_index"`         // key pool 中选中的 key index
    KeyMask         string    `json:"key_mask"`          // key 脱敏显示

    // === META: 上游 ===
    UpstreamURL     string    `json:"upstream_url"`      // 完整上游 URL
    UpstreamRequestID string  `json:"upstream_request_id"` // 上游返回的 request id
    UpstreamStatus  int       `json:"upstream_status"`   // 上游 HTTP status
    UpstreamHeaders map[string]string `json:"upstream_headers"` // 上游响应头

    // === META: 结果 ===
    StatusCode      int       `json:"status_code"`       // 返回给客户端的 status
    DurationMS      int64     `json:"duration_ms"`       // 总耗时
    InputTokens     int64     `json:"input_tokens"`      // 输入 token 数
    OutputTokens    int64     `json:"output_tokens"`     // 输出 token 数
    CachedTokens    int64     `json:"cached_tokens"`     // 缓存 token 数

    // === 内容 ===
    RequestBody     []byte    `json:"request_body"`      // 完整请求 body
    ResponseBody    []byte    `json:"response_body"`     // 完整响应 body（截断至 max_body_size）
}
```

## 架构

```
请求路径（同步）                         Trace Store（异步）
─────────────────                       ─────────────────
ServeHTTP
  ├─ readAndBufferBody ───────────────┐
  ├─ selectTarget                     │
  ├─ forwardRequest                   │
  ├─ writeResponse                    │
  │   └─ capture response body ───────┤
  └─ return                           │
                                      │
                               ┌──────▼──────┐
                               │  Ring Buffer │  ← 内存，最近 N 条
                               │  (sync.Map)  │     key=request_id
                               └──────┬──────┘
                                      │
                               ┌──────▼──────┐
                               │   channel   │  ← buffered channel
                               │  (buffered) │
                               └──────┬──────┘
                                      │
                               ┌──────▼──────┐
                               │  goroutine  │  ← 异步落盘
                               │   writer    │
                               └──────┬──────┘
                                      │
                               ┌──────▼──────┐
                               │  trace.db   │  ← 磁盘（BadgerDB / NDJSON）
                               └─────────────┘
```

## 配置

在 `config.json` 中新增 `trace_store` 字段：

```json
{
  "trace_store": {
    "enabled": false,
    "ring_buffer_size": 1000,
    "max_body_size": 2097152,
    "disk_path": "/var/lib/llms-proxy/trace.db",
    "disk_max_size_mb": 1024,
    "disk_ttl_hours": 24,
    "channel_buffer": 500
  }
}
```

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | `false` | 是否启用（仅 DEBUG 时开启） |
| `ring_buffer_size` | int | `1000` | 内存 Ring Buffer 容量（条数） |
| `max_body_size` | int | `2097152` (2MB) | 单个 body 最大字节数 |
| `disk_path` | string | `/var/lib/llms-proxy/trace.db` | 磁盘存储路径 |
| `disk_max_size_mb` | int | `1024` (1GB) | 磁盘存储最大 MB |
| `disk_ttl_hours` | int | `24` | 磁盘记录 TTL（小时） |
| `channel_buffer` | int | `500` | 异步写入 channel buffer |

## 查询 API

### Admin API

```
GET /admin/data/trace/:request_id     # 按 request_id 查询单条
GET /admin/data/trace?client=孙涛&limit=10  # 按客户端查询
GET /admin/data/trace?target=百炼TokenPlan&since=2026-05-27T05:00:00Z  # 按 target + 时间
```

### 查询逻辑

1. 先查内存 Ring Buffer（O(1)）
2. miss 则查磁盘（BadgerDB / 文件扫描）
3. 返回完整 TraceRecord

## 性能影响

| 场景 | 影响 |
|------|------|
| `enabled: false` | **零开销**（仅一次 bool 检查） |
| `enabled: true` | **极低**（内存写入 + channel 投递，不阻塞请求） |

**关键**：
- 内存 Ring Buffer 写入：sync.Map.Store，纳秒级
- Channel 投递：非阻塞 select，满则丢弃
- 磁盘写入：独立 goroutine，不影响请求路径

## 实现计划

### Phase 1: 内存 Ring Buffer
- `internal/tracestore/store.go` — Ring Buffer + channel + goroutine
- `internal/config/config.go` — TraceStoreConfig
- `internal/proxy/service.go` — 集成 TraceStore

### Phase 2: 异步落盘
- 磁盘存储（BadgerDB 或 NDJSON）
- TTL 自动清理
- 磁盘大小限制

### Phase 3: Admin API
- 查询接口
- 统计信息（内存/磁盘条数、丢弃计数）

## 与现有系统的关系

| 系统 | 内容 | 存储 | 触发条件 |
|------|------|------|----------|
| access.log | 请求元数据（无 body） | lumberjack | 所有请求 |
| upstream-error.log | 上游错误 + 响应摘要 | lumberjack | status >= 400 |
| usage_events | 用量统计 | bbolt | 成功请求 |
| **trace_store** | **完整 META + body** | **内存 + 磁盘** | **DEBUG 模式** |
