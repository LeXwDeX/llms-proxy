# 上游错误日志埋点设计（llms-proxy 侧）

> 配套文档：MeshyAI 侧实现见 `MeshyAI/docs/upstream-error-logging.md`。
> 两侧通过 **X-Request-Id** 关联，文件名统一为 `upstream-error.log`。

## 1. 目标

复盘 `gpt-img-3013cd81`（2026-04-29 07:32:01 上游网宿 502，耗时 162.79s）暴露的问题：

- llms-proxy access log 只有汇总行（status / duration），**没有上游响应 body 片段**
- 客户端拿到的 `"HTTP 502"` 与 llms-proxy 内部的 `request_id` 没有显式映射通道
- 排查需要人肉对齐时间窗，时间戳偏差 12 分钟就完全错位

本设计**不替换** access log，只在其旁边增加一份**结构化、专给上游错误**的文件，便于事后秒级定位。

## 2. 触发条件

写入 `upstream-error.log` 的事件：

| 类型 | 条件 |
|---|---|
| 上游业务错误 | 上游 HTTP `status >= 400`（含 4xx 与 5xx） |
| 上游连接错误 | `forwardRequest` 内部抛出 net error（DNS / connect refused / TLS / read reset） |
| 服务端异常 | proxy panic / config 校验失败 / target 池清空导致 503 |

**不写入**：客户端主动 abort（context canceled 且未触达上游）、healthz / admin 自身路由的非代理错误。

**4xx 入库说明**：用户决策包含 4xx 是为追踪 401（鉴权失效）、403（quota）、404（路径错）、422（参数被上游拒）等真实排障线索。**不做限流**——若日后出现写爆，排期独立优化。

## 3. Trace ID 协议

- **请求方负责生成**（MeshyAI 在 fetch 前生成 UUIDv4，放在 `X-Request-Id`）
- **llms-proxy 行为**：
  - 入口 middleware 检查 `X-Request-Id`；存在且符合 `^[A-Za-z0-9_-]{8,128}$` 则**复用**，否则**生成自己的**（保留现有逻辑）
  - 复用时与原有 `request_id` 字段语义合并，access log 与 error log 都用这个 id
  - 响应头**始终回写** `X-Request-Id`，便于客户端记入自己的错误日志
- 不引入 W3C trace context（traceparent 等），保持轻量

## 4. 日志路径与文件

```
/var/log/llms-proxy/upstream-error.log        # 当前
/var/log/llms-proxy/upstream-error.log.1.gz   # 昨日
/var/log/llms-proxy/upstream-error.log.2.gz   # 前日
...                                            # 共 30 份
```

容器内挂载点保持与现有 `error.log` / `access.log` 同目录。

## 5. 行格式（NDJSON，一行一条）

```json
{"ts":"2026-04-29T07:32:01.696Z","level":"error","trace_id":"7c2a...","kind":"upstream_5xx","method":"POST","path":"/v1/images/edits","client_ip":"172.18.0.2","target":"OpenAI-Image-Edits","endpoint_type":"wangsu_openai","upstream_url":"https://...","upstream_status":502,"duration_ms":162790,"req_bytes":4823,"resp_bytes":49,"resp_excerpt":"<!DOCTYPE html>..."}
```

### 字段约束

| 字段 | 必填 | 说明 |
|---|---|---|
| `ts` | ✓ | ISO8601 UTC 毫秒精度 |
| `trace_id` | ✓ | 与 access log 的 `request_id` 同值 |
| `kind` | ✓ | 枚举：`upstream_4xx` / `upstream_5xx` / `upstream_net_error` / `proxy_panic` / `target_pool_empty`。其中 408/429 归入 `upstream_4xx`（按 HTTP 标准） |
| `target` | ✓ | config.json 中的 target name |
| `endpoint_type` | ✓ | 便于按 capability 聚合统计 |
| `upstream_url` | ✓ | 实际打过去的完整 URL（含 path，去 query 中的 key） |
| `upstream_status` | net error 时填 0 | |
| `duration_ms` | ✓ | 从进入 forwardRequest 到收到响应/抛错 |
| `resp_excerpt` | net error 时省略 | 上游响应 body **前 1024 字节**，二进制按 hex；HTML 截断 |
| `error` | net error 必填 | Go error 字符串（classifyTransportError 后） |

### 不收集

- 上游请求 body（避免泄露用户 prompt / 图片 base64）
- 上游响应完整 body
- 任何 `Authorization` 头

## 6. 实现要点

| 位置 | 改动 |
|---|---|
| `internal/middleware/middleware.go` | 已有 `RequestID()` 中间件支持 `X-Request-ID` 复用（零改动）；header 名为 `X-Request-ID`（与设计文档统一） |
| `internal/errorlog/`（新包） | 提供 `Init(path) error` + `Write(entry)`，内部用 lumberjack 自轮转 + sync.Mutex 同步追加 |
| `internal/proxy/forward.go` `writeResponse` | 在 status >=400 分支写错误日志（原有 access log 与 warn 不动） |
| `internal/proxy/forward.go` `handleForwardError` | 调用 errorlog.Write 写 `upstream_net_error` |
| `internal/middleware/middleware.go` `Recoverer` | panic 路径写 `proxy_panic` |
| `cmd/proxy/main.go` | 启动时 `errorlog.Init(os.Getenv("UPSTREAM_ERROR_LOG_PATH") || "/var/log/llms-proxy/upstream-error.log")`；失败 warn + noop |

## 7. 轮转策略（lumberjack 内置）

复用项目现有 `internal/logging` 模式，**用 lumberjack 内轮转**，不依赖外部 logrotate（避免与 lumberjack 打架）。

| 参数 | 值 |
|---|---|
| MaxSize | 20 MB（与现有 access/error log 一致） |
| MaxBackups | 30 |
| MaxAge | 30 天 |
| Compress | true |

环境变量 `UPSTREAM_ERROR_LOG_PATH` 可覆盖，默认 `/var/log/llms-proxy/upstream-error.log`。打不开时 `slog.Warn` 记录原因后**降级为 noop**，不阻断启动。

## 8. 查询用法（人肉 grep）

```bash
# 按 trace id 反查（与 MeshyAI 错误日志对齐）
jq -c 'select(.trace_id=="7c2a...")' /var/log/llms-proxy/upstream-error.log

# 按 target + 时间窗
jq -c 'select(.target=="OpenAI-Image-Edits" and .ts>="2026-04-29T07:00:00Z")' upstream-error.log

# 统计某 target 的 5xx 分布（最近一天）
jq -r '.upstream_status' upstream-error.log | sort | uniq -c

# 找慢请求（>60s）
jq -c 'select(.duration_ms>60000)' upstream-error.log

# 跨日（含历史）
zcat upstream-error.log.*.gz | jq -c 'select(.target=="OpenAI-Image-Edits")'
```

## 9. 不做的事

- 不做 UI 查询页（用户决策：grep / SQL 即可）
- 不入数据库
- 不做阈值告警（单独排期）
- 不替换现有 access log / error log
- 不做 W3C trace context
- 不做异步队列（进程内同步追加足够）
- 不做错误类型分类标签的 wing/room 元数据（用户决策：什么都不要）

## 10. 与 capability-routing-design.md 的关系

`upstream_5xx` 这个 kind 与 capability-routing-design.md §4 的 canonical_reason `upstream_5xx_passthrough` 一一对应。后续 capability 路由实施时，error log 多写一个字段 `canonical_reason` 即可对齐归一化层。
