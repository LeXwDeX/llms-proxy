# Trace Logging — 全量请求/响应记录

## 目标

为 DEBUG 场景提供完整的请求体和响应体记录，独立于主数据库（bbolt），使用高性能文件存储。

## 设计原则

1. **独立存储**：不写入 bbolt，避免影响主数据路径性能
2. **异步写入**：通过 channel + goroutine 非阻塞写入，不影响请求延迟
3. **可配置**：开关、采样率、body 大小限制均可配置
4. **可查询**：提供 Admin API 按 request_id / client / target / 时间范围查询
5. **自动轮转**：lumberjack 轮转，与 errorlog 模式一致

## 存储格式

NDJSON（每行一个 JSON 对象），文件路径：`/var/log/llms-proxy/trace.log`

```json
{
  "ts": "2026-05-27T05:50:42.275Z",
  "trace_id": "174e35bf-da12-4823-8b72-25912820ff45",
  "client": "孙涛",
  "client_ip": "192.168.33.83",
  "target": "百炼TokenPlan",
  "endpoint_type": "bailian",
  "model": "qwen3.7-max",
  "method": "POST",
  "path": "/v1/chat/completions",
  "upstream_url": "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions",
  "upstream_request_id": "6e5fab24-9aea-4f08-bfba-b1a7ae079c01",
  "status_code": 200,
  "duration_ms": 582,
  "req_bytes": 300337,
  "resp_bytes": 112,
  "request_body": "{\"model\":\"qwen3.7-max\",\"messages\":[...]}",
  "response_body": "{\"id\":\"chatcmpl-xxx\",\"choices\":[...]}",
  "key_index": 0,
  "key_mask": "sk-sp-D.***EU="
}
```

## 配置

在 `config.json` 中新增 `trace_log` 字段：

```json
{
  "trace_log": {
    "enabled": true,
    "path": "/var/log/llms-proxy/trace.log",
    "max_body_size": 1048576,
    "sample_rate": 1.0,
    "max_size_mb": 100,
    "max_backups": 10,
    "max_age_days": 7,
    "compress": true
  }
}
```

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | `false` | 是否启用 trace log |
| `path` | string | `/var/log/llms-proxy/trace.log` | 日志文件路径 |
| `max_body_size` | int | `1048576` (1MB) | 单个 body 最大字节数，超出截断 |
| `sample_rate` | float | `1.0` | 采样率 0.0-1.0，1.0 = 全量记录 |
| `max_size_mb` | int | `100` | 单个日志文件最大 MB |
| `max_backups` | int | `10` | 保留的旧日志文件数 |
| `max_age_days` | int | `7` | 日志保留天数 |
| `compress` | bool | `true` | 是否 gzip 压缩旧日志 |

## 架构

```
请求路径（同步）                    Trace Log（异步）
─────────────────                  ─────────────────
ServeHTTP
  ├─ readAndBufferBody ──────────┐
  ├─ selectTarget                │
  ├─ forwardRequest              │
  ├─ writeResponse               │
  │   └─ capture response body ──┤
  └─ return                      │
                                 │
                          ┌──────▼──────┐
                          │   channel   │
                          │  (buffered) │
                          └──────┬──────┘
                                 │
                          ┌──────▼──────┐
                          │  goroutine  │
                          │   writer    │
                          └──────┬──────┘
                                 │
                          ┌──────▼──────┐
                          │  trace.log  │
                          │  (NDJSON)   │
                          └─────────────┘
```

## 实现要点

### 1. 异步写入

```go
type TraceEntry struct {
    // ... fields
}

type TraceLogger struct {
    ch     chan TraceEntry
    writer io.Writer
    done   chan struct{}
}

func (t *TraceLogger) Write(entry TraceEntry) {
    select {
    case t.ch <- entry:
    default:
        // channel full, drop entry (non-blocking)
    }
}

func (t *TraceLogger) run() {
    for entry := range t.ch {
        // marshal and write to file
    }
}
```

### 2. Body 捕获

- **Request body**：在 `ServeHTTP` 开头 `readAndBufferBody` 时已读取，直接传递
- **Response body**：在 `writeResponse` 中使用 `limitedCaptureWriter`（已有，限制 2MB）

### 3. 采样

```go
if config.SampleRate < 1.0 {
    if rand.Float64() > config.SampleRate {
        return // skip
    }
}
```

### 4. Admin API

```
GET /admin/data/trace?request_id=xxx
GET /admin/data/trace?client=孙涛&limit=10
GET /admin/data/trace?target=百炼TokenPlan&since=2026-05-27T05:00:00Z
```

实现：读取 trace.log 文件，按条件过滤，返回匹配的 entries。

## 性能影响

| 场景 | 影响 |
|------|------|
| `enabled: false` | 零开销（仅一次 bool 检查） |
| `enabled: true, sample_rate: 0.01` | 极低（1% 请求异步写入） |
| `enabled: true, sample_rate: 1.0` | 低（全量异步写入，channel buffer 吸收峰值） |

**关键**：写入操作在独立 goroutine，不阻塞请求路径。channel 满时丢弃 entry（降级为采样）。

## 与现有日志的关系

| 日志 | 内容 | 触发条件 | 存储 |
|------|------|----------|------|
| access.log | 请求元数据（无 body） | 所有请求 | lumberjack |
| error.log | 应用错误 | 错误事件 | lumberjack |
| upstream-error.log | 上游错误 + 响应摘要 | status >= 400 | lumberjack |
| **trace.log** | **完整请求/响应 body** | **配置启用** | **lumberjack** |
| usage_events | 用量统计 | 成功请求 | bbolt |

## 使用场景

1. **DEBUG 特定请求**：通过 request_id 查询完整的请求/响应内容
2. **分析模型行为**：查看发送给模型的完整 prompt 和模型返回
3. **排查上游问题**：对比请求体和响应体，定位上游 API 问题
4. **审计**：记录所有请求内容（需配合合规要求）

## 注意事项

1. **隐私**：trace log 包含完整的请求/响应内容，可能包含敏感信息。生产环境建议：
   - 仅在 DEBUG 时临时启用
   - 设置较短的 `max_age_days`
   - 限制文件权限（600）

2. **磁盘空间**：全量记录会产生大量数据。建议：
   - 使用 `sample_rate` 控制写入量
   - 设置合理的 `max_size_mb` 和 `max_backups`
   - 监控磁盘使用

3. **性能**：虽然异步写入，但高并发下 channel 可能满。建议：
   - 设置合理的 channel buffer size（默认 1000）
   - 监控 channel 满的丢弃计数
