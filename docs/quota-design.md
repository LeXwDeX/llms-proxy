# 账户配额限额设计文档

> **状态：草案 v1**，待评审。本文档不定实现细节，只约束架构边界、设计模式、功能契约。
> 关联：`AGENTS.md`（全局架构）、`docs/capability-routing-design.md`（路由层独立，与本设计无交叉）。
> 完成模块后按 `doc-lifecycle` 规则退化至稳态简述。

---

## 1. 背景（一句话说清）

让代理服务像一个"外挂审计员"，异步观测每个客户端的累计 USD 用量；超限时向系统发送打断指令中断当前 SSE 流，并在该客户端的下一次请求起直接拒绝（429），不再影响主转发路径性能。

### 1.1 适用范围（重要）

本限额系统**仅针对根目录的 LLM 转发代理链**，即：

- 路由前缀为 `/chat/`, `/messages/`, `/gemini/`, `/images/`, `/audio/`, `/embeddings/`, `/deepseek/` 等 capability 路由；
- 或 catch-all 路径透传到非 copilot 上游的请求。

**明确排除**（不在本系统范围内）：

| 模块 | 排除理由 |
|------|----------|
| GitHub Copilot 转发代理（`/copilot/*` 路径） | 已有独立的 `copilot.QuotaManager`（premium request 计数），与 USD 计费是两套不同体系，叠加会导致重复计费 |
| 模型名带 `Copilot ` 前缀的请求（走 `handleCopilotRequest`） | 同上，模型名前缀是 Copilot 通道标识，应走 premium request 计费 |

### 1.2 隔离点

quota Manager 的 `Check` / 超限标记 / SSE 中断链路**不得**介入 Copilot 请求路径：

- `cmd/proxy/main.go` 中 `/copilot/*` 路由由 chi router 单独挂载，**不经过** `proxy.Service.ServeHTTP`，因此 quota Manager 的准入检查天然不生效。
- `/copilot/*` 路由使用 `proxy.Service.HandleCopilotAuth/QuotaSummary/Models/Passthrough` 四个独立 handler，不触发 `recordUsageEvent`，因此 quota Manager 的 Evaluate 也不参与。
- 模型名带 `Copilot ` 前缀的请求若在 `ServeHTTP` 内被识别并分流到 `handleCopilotRequest`，quota Manager 的 Check 必须在该分流**之后**执行，或在 Check 函数内主动跳过 Copilot 模型。

**约束**：在 proxy.ServeHTTP 中，quota Manager 的 Check 调用必须位于"识别并分流 Copilot 请求"**之后**；或者 Check 函数内部检测请求模型并豁免 Copilot 前缀。优先选择前者（更干净）。

---

## 2. 设计原则

> 与 `AGENTS.md` 全局协议对齐：

- **性能零妥协**：配额评估不在请求热路径（不在 ServeHTTP 同步链路中调用），不读 DB，不计算费用。
- **异步观察者模型**：配额系统作为外挂层存在，对主转发仅暴露 1 个接口：`Check(client) bool`（查内存标记）。
- **延迟生效优先于强一致**：超限在"下次请求"时才生效，允许当前 SSE 流自然跑完并产生轻微超额。
- **DB 是配置的单一来源**：Client 配额配置只存 DB；超限标记只存内存，重启从 DB 读取 + 重算标记。
- **自然周期自动重置**：日/周/月边界到来时内存标记自动失效，无需定时器或人工解封。

---

## 3. 核心抽象：Manager

`internal/quota.Manager` 是本模块唯一的对外表面。它承担四个职责，且每个职责有明确的调用方：

| 职责 | 方法 | 调用方 | 时机 |
|---|---|---|---|
| 准入检查 | `Check(clientName) (ExceededInfo, bool)` | `proxy.Service.ServeHTTP` | 认证通过后，进入 selectTarget 之前 |
| 异步评估 | `Evaluate(clientName)` | 内部定时器 + 启动时批量 | 由 Manager 内部调度，外部不主动调用 |
| 配额进度查询 | `Status(clientName) QuotaStatus` | `admin.Handler` 的 `/admin/data/quota/{client}` | 按需 |

**禁止**：
- 禁止 Manager 暴露 `EvaluateSync` 类同步评估接口供代理层调用。
- 禁止 Manager 持有任何 `proxy.Service` / `http.Response` 的引用（方向反转：Service 持有 Manager）。

---

## 4. 依赖方向（架构约束）

```
cmd/proxy/main.go
        │
        ├── 初始化 Manager(catalog, modelCostStore, usageAggStore, clientStore)
        │
        └── 注入到 Service 和 admin.Handler
                       │
                       ▼
internal/quota（本模块）
        │
        ├── 读：internal/catalog（取模型单价）
        ├── 读：internal/nosql.ModelCostStore（取手工覆盖价）
        ├── 读：internal/nosql.UsageStore（agg_hourly 聚合查询）
        └── 读：internal/nosql.ClientStore（取客户端配额配置）

不允许：quota → proxy / quota → auth / proxy 直接操作 quota 内部状态
```

Manager 通过 `*catalog.Catalog` + `ModelCostStore` + `UsageStore` 三个只读依赖完成费用计算，**不引入新 DB、不复制数据**。

---

## 5. 数据模型约束

### 5.1 Client 配置扩展（DB 持久化）

`config.Client` 结构（位于 `internal/config/config.go`，非 `nosql` 包）新增 optional 字段，向后兼容（`omitempty`）：

| 字段 | 类型 | 含义 | 单位 |
|---|---|---|---|
| `quota_daily_usd` | float64 | 自然日限额 | USD，`0` 表示不限制 |
| `quota_weekly_usd` | float64 | 自然周限额 | USD，`0` 表示不限制 |
| `quota_monthly_usd` | float64 | 自然月限额 | USD，`0` 表示不限制 |

**约定**：
- 字段命名采用 snake_case 以与现有 `config.Client` 序列化风格一致。
- 任何维度的 `0` 值 = 不限制，等价于不配置。
- 三个维度可独立配置，互不干扰；任一维度超限即生效。
- `nosql.ClientStore.List()` / `ListWithQuota()` 均返回 `[]config.Client`（不引入新 Client 类型）。

### 5.2 内存超限标记（不持久化）

Manager 内部维护：

```
map[clientName] → ExceededInfo{
    Dimension   string    // "daily" | "weekly" | "monthly"
    Limit       float64   // 触发的限额值
    Used        float64   // 评估时的累计用量
    ResetsAt    time.Time // 下个自然周期起始
}
```

- 一个 client 同时只保留**最先触发**的那个维度（如日超限先于月超限，则仅记日）。
- 标记可被 `Evaluate` 更新（如日重置后月仍然超，则替换为月）。
- 标记可被 `Check` 自动清除（检测到 `ResetsAt ≤ now`）。

---

## 6. 自然周期计算规范（功能模块约束）

| 维度 | 周期起始 | 重置时刻 | 聚合区间 |
|---|---|---|---|
| `daily` | `today 00:00:00 UTC` | `tomorrow 00:00:00 UTC` | `[today 00:00, now]` |
| `weekly` | **本周一（ISO 8601，周一=第 1 天）** `00:00:00 UTC` | **下周一** `00:00:00 UTC` | `[本周一 00:00, now]` |
| `monthly` | `本月 1 日 00:00:00 UTC` | `下月 1 日 00:00:00 UTC` | `[本月 1 日 00:00, now]` |

**约束**：
- 时区固定为 **UTC**，不受服务器本地时区影响。
- 周期起止时间必须通过 `time.Time` 的日期构造方法计算，**禁止**用字符串拼接后 `Parse`。
- `ResetsAt` 在 Manager 写入标记时必须精确到下次周期起始，供 `Check` 自动解封和 429 响应体使用。

---

## 7. 聚合查询接口契约

### 7.1 UsageStore 新增方法

```
UsageStore.SumByClientRange(client string, from time.Time, to time.Time)
    → map[endpointType:model]UsageTotals

UsageTotals = { InputTokens int64, OutputTokens int64, CachedTokens int64 }
```

**约束**：
- 必须基于 `usage_agg_hourly` bucket 做 cursor range scan，**禁止**扫描 `usage_events`。
- key 格式 `YYYYMMDDHH|endpoint_type|client|model` 直接按前缀 + 时间范围过滤。
- 返回 **`endpoint_type:model` 为键的分组结果**，因为不同模型的费率不同，需要先分组再乘单价。
- `from` / `to` 由 Manager 按 §6 的周期规范计算后传入，本方法不做周期推断。

### 7.2 费用换算

Manager 拿到分组结果后，对每个 `(endpoint_type, model)` 调用现有 `CostTable.LookupCost(endpointType, model)`：

```
groupCost = Σ( for each (ep,model) :
    input/1M × rate.input + output/1M × rate.output + cached/1M × rate.cached )
```

**CostTable 构建位置**：`ToCostTable(costs []nosql.ModelCost, cat *catalog.Catalog) usage.CostTable` 放在独立包 **`internal/costutil/cost_table.go`**（不可放在 `usage` 包——`nosql` 已 import `usage`，反向 import 将形成 Go 循环依赖；也不可放在 `nosql` 包——`nosql` 不应感知 catalog）。本包单向依赖 `catalog` / `nosql` / `usage`，无循环。`admin` 和 `quota` 均通过 `costutil.ToCostTable(...)` 调用。

---

## 8. 评估触发与时序（设计模式约束）

架构：**memory-first + event-driven + hourly DB calibration**。无 5 秒轮询，无全量重算 ticker。

### 8.1 内存优先增量 + 惰性周期翻转

- **Increment**：请求完成时把本次费用累加到内存计数器（零 DB 命中），超限即更新内存 exceeded 标记。
- **Check**：准入时只读内存 exceeded 标记（零 DB 命中）。
- **Lazy period flip**：Increment / Check 检测到 `ResetsAt ≤ now` 时立即清零该维度计数器，消除"周期边界到来"与"下次校准"之间的亚秒级空窗。

### 8.2 每小时 DB 校准 + 启动预加载

- **Evaluate**：单 client 的权威 DB 聚合（基于 `usage_agg_hourly`），用于三处：启动预加载、admin 配置变更、每小时校准。
- **Hourly calibration**：通过 `time.AfterFunc` 对齐自然小时边界（00:00、01:00…），每个边界对所有 `quota_*_usd > 0` 的 client 执行 `Evaluate`。日/周/月周期边界都落在小时边界上，故校准天然处理周期重置并修正累积漂移。
- **启动预加载**：`Manager.Start()` 阶段 `ClientStore.ListWithQuota()` 取全部配置配额的 client，串行 `Evaluate` 建立初始标记；超时上限 10s，超过记 WARN 并继续启动（不阻塞进程）。

**约束**：
- 校准定时器为 Manager 独占，不共享、不借用。
- 单 client 不得并发 Evaluate（内部按 client 加轻量锁）。

### 8.3 Shutdown

Manager.Stop()：
- 停止定时器和校准定时器。

---

## 9. 中断机制（已废弃）

> 本节描述的 SSE 流中途 TCP RST 中断机制已于 2026-06-08 废弃。
> 当前策略：quota 超限仅通过准入检查（请求前 429）拦截，已在跑的 SSE 流让其自然跑完。
> 原 §9 的 Hijack + activeStreams + TCP RST 相关代码（`quota_hijack.go`、`forward.go` SSE Hijack 分支、`Manager.RegisterActiveStream`、`nextStreamID`）已删除。

---

## 10. 准入检查与 429 响应格式（功能模块约束）

### 10.1 检查逻辑

`Manager.Check(clientName)`：
1. 读内存标记 → 不存在 → 返回 `false`（放行）。
2. 存在 → 检查 `ResetsAt`：
   - `ResetsAt ≤ now` → 清除标记，返回 `false`（自动解封）。
   - `ResetsAt > now` → 返回 `(ExceededInfo, true)`（拒绝）。

### 10.2 429 响应体（OpenAI 兼容）

```json
{
  "error": {
    "message": "Quota exceeded (<dimension>). Limit: $<limit>, used: $<used>. Resets at <ISO8601 UTC>",
    "type": "quota_exceeded",
    "code": "429",
    "quota": {
      "dimension": "daily" | "weekly" | "monthly",
      "limit_usd": 10.00,
      "used_usd": 10.23,
      "resets_at": "2026-06-09T00:00:00Z"
    }
  }
}
```

HTTP status = `429 Too Many Requests`。**禁止**用 402 / 503 / 400。

### 10.3 调用位置

`proxy.Service.ServeHTTP` 中 auth 校验通过后、`selectTarget` 之前：

```
if info, exceeded := quotaManager.Check(principal.Name); exceeded {
    渲染 429 响应（JSON + Content-Type application/json）
    return
}
```

---

## 11. Admin 接口（功能模块约束）

### 11.1 Client 配额配置

复用现有 `PUT /admin/data/clients/{name}`，body 增加 3 个 optional 字段（`quota_daily_usd` / `quota_weekly_usd` / `quota_monthly_usd`）。

### 11.2 配额进度查询（新增）

```
GET /admin/data/quota/{client}
```

响应示例：

```json
{
  "client": "yc0868",
  "quotas": {
    "daily":   {"limit": 10.00, "used": 3.50,  "resets_at": "2026-06-09T00:00:00Z"},
    "weekly":  {"limit": 50.00, "used": 22.30, "resets_at": "2026-06-15T00:00:00Z"},
    "monthly": {"limit": 200.0, "used": 87.60, "resets_at": "2026-07-01T00:00:00Z"}
  },
  "exceeded": {
    "dimension": "daily",
    "limit": 10.00,
    "used": 10.23,
    "resets_at": "2026-06-09T00:00:00Z"
  }
}
```

- `quotas.*` 始终返回三个维度（即使 limit=0 也返回，used 为累加值）。
- `exceeded` 字段：未超限为 `null`，超限时返回与内存标记相同结构。

---

## 12. 与现有模块的对接边界

### 12.1 复用项（不改）

| 项 | 复用方式 |
|---|---|
| `usage_agg_hourly` 存储 | 仅读，新增聚合查询方法 |
| `CostTable` / `LookupCost` / `InferOriginalEndpointType` | 直接调用 |
| `catalog.Catalog` 模型价 | 直接调用 |
| `nosql.ClientStore` CRUD | 仅扩展字段 |
| `recordUsageEvent` 主流程 | 仅在成功 Record 后发信号或不做任何操作 |
| admin.Handler 的 client CRUD | 仅扩展 body 字段 |

### 12.2 新增项

| 项 | 位置 |
|---|---|
| `internal/quota/manager.go` | 核心 Manager |
| `internal/quota/period.go` | 自然周期计算（§6） |
| `internal/quota/types.go` | ExceededInfo / QuotaStatus 等公共类型 |
| `internal/costutil/cost_table.go` | `ToCostTable(costs []nosql.ModelCost, cat *catalog.Catalog) usage.CostTable`（独立 util 包，避免 nosql↔usage 循环依赖） |
| `internal/nosql/usage_agg.go` | `SumByClientRange` 方法 + `UsageTotals` 类型 |
| `internal/nosql/clients.go` | `ListWithQuota` 方法（返回 `[]config.Client`） |
| `admin.Handler.handleQuotaStatus` | 新增路由 |
| `cmd/proxy/main.go` | Manager 初始化、注入、Start/Stop 编排 |

### 12.3 唯一侵入点

| 文件 | 改动 |
|---|---|
| `proxy/service.go::ServeHTTP` | 加一行 Check 判断 429 早返回 |

> 注：quota 系统仅作为准入检查外挂，不侵入转发链路（io.Copy 调用语义完全不变）。

---

## 13. 实施分期

### Phase 1：约束基线（本阶段目标）

- 本文档定稿；
- 落地骨架接口：`Manager` / `ExceededInfo` / `QuotaStatus` 公开类型签名 + `SumByClientRange` 接口签名；
- archgate 校验 PASS。

### Phase 2：数据层与周期计算

- `nosql.Client` 增字段 + 序列化兼容测试；
- `usage_agg` 聚合查询 + 单元测试（空数据 / 跨周期边界 / 多模型多 epType 混合）；
- `period.go` 自然周期计算 + 时区正确性单测（跨日 / 跨周 / 跨月 / UTC 边界）。

### Phase 3：Manager 核心

- `Manager` 启动 / 批量预加载 / 定时器评估 / Shutdown；
- `Check` 自动解封单测（模拟周期切换）；
- `Evaluate` 多 client 并发安全单测；
- `costutil.ToCostTable` 落地（独立包）+ admin 现有 `toUsageCostTable` 迁移为调用 `costutil.ToCostTable` + 回归 admin 现有测试。

### Phase 4：接入主链路

- `proxy/service.go` 注入 Manager，加 429 早返回 + 集成测试；
- `admin.Handler` 新增 `/quota/{client}` + 测试。

### Phase 5：灰度与观察

- 33.110 灰度部署；
- 观察 1 周，无误判（不该超的被超）或漏判（应超的未超）。

---

## 14. 验收标准

### 性能

- 未配置 quota 的 client，请求链路与基线相比延迟 < 1ms（P99）。
- Manager 定时器评估不阻塞任何 goroutine（包括 HTTP handler）。

### 功能

- 超限 client 的下次请求返回 429 + 标准 JSON 错误体。
- 已在跑的 SSE 流不受 quota 影响，让其自然跑完（quota 是软限制）。
- 自然周期切换瞬间内存标记自动失效，下次请求放行。
- 服务重启后 10s 内，所有应超的 client 标记被重建。

### 数据正确性

- 同一 client 在 `GET /admin/data/usage/summary` (admin 现有) 与 `GET /admin/data/quota/{client}` 给出的 USD 用量数值一致（同源 CostTable）。
- `quota_daily_usd = 0` 等价于"不启用日限额"，不被误算为限额 0。

### 兼容性

- 未配置 quota 的 client 行为零变化；
- 现有 admin client CRUD 接口向后兼容（未提供 quota 字段则保持原值）。

---

## 15. 风险与缓解

| 风险 | 缓解 |
|---|---|
| **`usage_agg_hourly` range scan 在大 client 数量下变慢** | 实测观测；必要时增加按 client 分 bucket 的索引 |
| **`toUsageCostTable` 重构导致 admin 现有测试回归** | 实施前跑通 admin 现有全部测试 |
| **误判（不该超的被超）**：CostTable 价格不准 / token 累加 bug | Admin `/quota/{client}` API 提供审计入口；误判可通过临时调高 quota 即时解除 |
| **漏判（应超的未超）**：定时器失效 / 聚合查询漏扫 | 定时器退出必须 panic/WARN，不静默失败 |

---

## 16. 不在本设计范围（后期迭代）

| 项 | 理由 |
|---|---|
| Token 总量限额（非 USD） | 本期仅 USD，避免依赖模型价格准确性 |
| 按模型/通道维度限额 | 聚合复杂度高，按 Client 维度已足够覆盖核心场景 |
| 软限额预警（80% 通知） | 依赖通知基础设施 |
| Webhook / 邮件告警 | 同上 |
| 批量配置（按 client 组） | 先验证单 client 流程 |
| 审计日志记录超限事件 | 后期接入 errorlog 框架 |
| Admin UI 配额进度卡 | 后端 API 稳定后再做前端 |
| SSE 协议层错误事件注入 | 本期采用 TCP RST 通杀方案；后期若需友好提示可叠加注入 `event: error`（兼容 RST 作为兜底） |
