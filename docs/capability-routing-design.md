# Capability 路由 + 错误归一化设计文档

> 状态：**草案 v1**，待评审。本文档不实现，只确定设计。
> 关联讨论：参见 `AGENTS.md` 与 `部署要求.md`，本设计是对现有 catch-all 路由的演进。

---

## 1. 背景与动机

### 1.1 现状

当前 llms-proxy 的入口路由（`cmd/proxy/main.go`）：

| 路径 | 行为 |
|---|---|
| `/healthz`、`/login`、`/admin/*` 等 | 系统/管理面，与本设计无关 |
| `/copilot/*` | Copilot 专用通道，独立服务链 |
| `/deepseek/*` | **唯一的前缀子路由**：剥前缀 + 注入 `EndpointTypeHint=deepseek` |
| `/*`（catch-all） | 按 `model` 名 + 客户端 path 自动选 target |

catch-all 决策依赖三件事：
1. body 里的 `model` 字段（`extractModel`）；
2. target 的 `allowed_models`（`modelAllowed`）；
3. `path_capability.go` 对 wangsu 终态 URL 的额外路径过滤。

### 1.2 暴露出来的问题

1. **路由意图隐式**：客户端不指定 endpoint_type，靠 model 名反推；同一 model 同时挂在多个 endpoint_type 时（如 `gpt-image-2` 同挂 azure_openai + wangsu_openai_image_edit），实际走哪条由 round-robin + affinity 决定，调试链路长。
2. **错误格式杂乱**：上游各家协议错误结构差异巨大，当前直接透传上游 body。客户端 SDK（按某一种协议解析）遇到跨协议响应会解析失败。
3. **失败判定只看 HTTP status**：`isUpstreamFailureStatus` 仅识别 5xx/408/429。Azure / Gemini 经常 200 + body 里 `finish_reason=content_filter`，wangsu 偶发 200 + 网关错；这些"伪 200" 不会触发 fallback，等价于把错误透传给客户端。
4. **业务错与容量错混在一起**：参数错、内容审核拒绝、欠费等业务错也会被 5xx/429/408 误归类时触发 fallback，N 个 target 全打一遍最后还是同一个错，浪费配额。

### 1.3 演进方向

引入两层抽象：

1. **Capability 分组前缀**：`/{capability}/...` 替代 catch-all。一个 capability 锁定一种对外协议（OpenAI / Anthropic / Google），组内允许多 endpoint_type 聚合负载。
2. **错误归一化（normalizer）**：每个 endpoint_type 一套 normalizer，输出统一的 `{retriable, business_error, canonical_reason, original}` 结构。所有 fallback 决策只看 normalizer 输出，对外响应按 capability 组锁定的协议格式渲染。

---

## 2. 设计原则

> 与 `AGENTS.md` 全局协议保持一致：

- **不破坏现有契约**：`/copilot/*`、`/deepseek/*`、`/admin/*` 行为不变。
- **聚合负载不能丢**：同 capability 跨 endpoint_type 的 fallback 必须保留。
- **业务错不 fallback**：normalizer 必须能区分 retriable vs business_error，避免 N 个 target 全炸同一个错。
- **对外协议固定**：客户端 SDK 进入某 capability 前缀后，永远只看到该协议的错误结构，不需要在 SDK 外加 polyfill。
- **零硬编码迎合**：normalizer 的判定规则全部以"上游官方文档定义的可恢复错误"为依据，不针对单 case 写魔法分支。
- **可观测优先于行为复杂**：fallback 链路里所有中间错误都通过 response header（`X-Proxy-Fallback-Chain`）+ 结构化日志暴露，不塞进 response body。

---

## 3. Capability 分组定义

### 3.1 分组表

| capability 前缀 | 对外协议 | 允许的 endpoint_type | 对应客户端 path（示例） | 当前覆盖的 target |
|---|---|---|---|---|
| `/chat/` | OpenAI | `openai` / `azure_openai` / `wangsu_openai` | `/chat/v1/chat/completions` | OpenAI-Chat、East US 2 |
| `/messages/` | Anthropic | `claude` / `wangsu_claude` | `/messages/v1/messages` | Claude |
| `/gemini/` | Google | `gemini` / `wangsu_gemini` | `/gemini/v1beta/models/{model}:generateContent` | Gemini |
| `/images/` | OpenAI | `wangsu_openai_image` / `wangsu_openai_image_edit` / `azure_openai`（dall-e/gpt-image 类）/ `openai` | `/images/v1/images/generations`、`/images/v1/images/edits` | OpenAI-Image-Gen、OpenAI-Image-Edits、East US 2 |
| `/audio/` | OpenAI | `azure_openai`（tts/whisper） / `openai` | `/audio/v1/audio/speech`、`/audio/v1/audio/transcriptions` | North Central US |
| `/embeddings/` | OpenAI | `azure_openai` / `openai` | `/embeddings/v1/embeddings` | East US 2 |
| `/deepseek/` | OpenAI + Anthropic 双协议 | `deepseek` | `/deepseek/v1/chat/completions`、`/deepseek/v1/messages` | DeepSeek（保持现状） |
| `/copilot/` | OpenAI | `copilot` | `/copilot/...` | Copilot 池（保持现状） |

> 说明：
> - `/deepseek/` 与 `/copilot/` 维持现状不动（已有专用处理链）。
> - 其余前缀对组内 target 做聚合负载选择。
> - 当前没有 target 的 capability（如 `/audio/` 只有 1 个）也仍以前缀暴露，组内只有 1 个 target 时退化为单点；新增 target 时无需改路由。

### 3.2 路径映射规则

客户端访问 `/chat/v1/chat/completions`，路由层做：
1. 剥前缀 `/chat`，内部 path 变为 `/v1/chat/completions`；
2. 注入 `CapabilityHint=chat` 到 context；
3. 透传给 `proxyService.ServeHTTP`，由现有 selectTarget 在 capability 允许的 endpoint_type 池内做选择。

不在分组表中的前缀返回 404；不在 capability 允许 path 列表中的请求返回 400（避免静默错路由）。

### 3.3 capability 允许的客户端 path 白名单

| capability | 允许的客户端 path（剥前缀后） |
|---|---|
| `/chat/` | `/v1/chat/completions`、`/chat/completions` |
| `/messages/` | `/v1/messages`、`/v1/messages/count_tokens` |
| `/gemini/` | `/v1beta/models/{model}:generateContent`、`/v1beta/models/{model}:streamGenerateContent`、`/v1/models/{model}:generateContent` |
| `/images/` | `/v1/images/generations`、`/v1/images/edits`、`/v1/images/variations` |
| `/audio/` | `/v1/audio/speech`、`/v1/audio/transcriptions`、`/v1/audio/translations` |
| `/embeddings/` | `/v1/embeddings` |

未列出的 path 直接 404，理由：禁止"上游有但我们没考虑过"的协议悄悄走代理。

---

## 4. 错误归一化（Normalizer）设计

### 4.1 抽象接口（伪代码）

```go
type NormalizedError struct {
    Retriable        bool        // 是否应触发 fallback（容量/网络/临时错）
    BusinessError    bool        // 是否业务错（参数错/内容审核/鉴权失败/欠费）
    CanonicalReason  string      // 归一化原因码（见 4.2）
    UpstreamStatus   int         // 上游原始 HTTP code
    UpstreamSnippet  string      // 上游原始 body 片段（截断 4KB，仅用于日志）
}

type Normalizer interface {
    // Normalize 检查上游响应（含 200 但 body 报错的情况）。
    // 入参：上游 status、Content-Type、body bytes、客户端 path。
    // 出参：归一化错误；若上游成功则返回 nil。
    Normalize(status int, contentType string, body []byte, path string) *NormalizedError
}
```

每个 endpoint_type 实现一个 Normalizer。注册表按 endpoint_type 索引。

### 4.2 Canonical Reason 码（统一对外）

| canonical_reason | 含义 | retriable | business_error | 触发 fallback |
|---|---|---|---|---|
| `upstream_overloaded` | 容量饱和（429 + "engine overloaded" / 529 / RESOURCE_EXHAUSTED） | true | false | 是 |
| `upstream_rate_limited` | 客户端配额限流（区别于容量过载） | false | true | 否 |
| `upstream_timeout` | 408 / 504 / 网关超时 | true | false | 是 |
| `upstream_5xx` | 通用 5xx | true | false | 是 |
| `upstream_network` | 传输层错误（连接被拒/DNS） | true | false | 是 |
| `content_filter` | 内容审核拒绝（Azure content_filter / Gemini SAFETY） | false | true | 否 |
| `invalid_request` | 参数错、模型不存在、路径错 | false | true | 否 |
| `unauthorized` | 401/403/api key 错 | false | true | 否 |
| `quota_exceeded` | 上游账户欠费/超额 | false | true | 否 |
| `model_not_found` | 模型在 target 上未找到 | false | true | 否 |
| `unknown` | 无法分类 | true | false | 是（保守 fallback） |

### 4.3 各 endpoint_type 归一化规则（草案）

#### 4.3.1 `azure_openai`

| 上游响应特征 | canonical_reason |
|---|---|
| status=429 且 body `code=="429"` 含 "rate limit" | `upstream_rate_limited` |
| status=429 且 body 含 "Engine is overloaded" / "model is currently overloaded" | `upstream_overloaded` |
| status=400 且 body `code=="content_filter"` | `content_filter` |
| status=400 且 body `code=="DeploymentNotFound"` 或 `code=="InvalidRequest"` | `invalid_request` |
| status=401/403 | `unauthorized` |
| status=404 且 path 含 `/deployments/` | `model_not_found` |
| status=408/504 / context 超时 | `upstream_timeout` |
| status>=500 | `upstream_5xx` |
| status=200 且 body `choices[0].finish_reason=="content_filter"` | `content_filter` |

#### 4.3.2 `wangsu_openai` / `wangsu_openai_image` / `wangsu_openai_image_edit` / `openai`

| 上游响应特征 | canonical_reason |
|---|---|
| status=429 且 body `error.code` 含 "rate_limit" | `upstream_rate_limited` |
| status=429 且 body `error.message` 含 "overloaded" | `upstream_overloaded` |
| status=400 且 body `error.type=="invalid_request_error"` | `invalid_request` |
| status=400 且 body `error.code=="content_policy_violation"` | `content_filter` |
| status=401/403 | `unauthorized` |
| status=402 或 body 含 "insufficient_quota" | `quota_exceeded` |
| status=404 且 body `error.code=="model_not_found"` | `model_not_found` |
| status=408/504 | `upstream_timeout` |
| status>=500 | `upstream_5xx` |
| **网宿网关层伪 200**：status=200 但 Content-Type 非 JSON 或 body 含 `{"code": ..., "msg": ...}` 非标准结构 | `upstream_5xx`（网关故障当上游故障处理） |

#### 4.3.3 `wangsu_claude` / `claude`

| 上游响应特征 | canonical_reason |
|---|---|
| status=529 或 body `error.type=="overloaded_error"` | `upstream_overloaded` |
| status=429 | `upstream_rate_limited` |
| status=400 且 body `error.type=="invalid_request_error"` | `invalid_request` |
| status=401/403 或 body `error.type=="authentication_error"` / `permission_error` | `unauthorized` |
| status=400 且 body `error.type=="not_found_error"` 含 model | `model_not_found` |
| status=408/504 | `upstream_timeout` |
| status>=500 | `upstream_5xx` |

#### 4.3.4 `wangsu_gemini` / `gemini`

| 上游响应特征 | canonical_reason |
|---|---|
| status=429 且 body `error.status=="RESOURCE_EXHAUSTED"` | `upstream_overloaded`（Gemini 不区分配额/容量，统一按容量过载处理 fallback） |
| status=400 且 body `error.status=="INVALID_ARGUMENT"` | `invalid_request` |
| status=401/403 或 body `error.status=="PERMISSION_DENIED"` / `UNAUTHENTICATED` | `unauthorized` |
| status=404 且 body `error.status=="NOT_FOUND"` 含 model | `model_not_found` |
| status=200 且 body `candidates[0].finishReason=="SAFETY"` 或 `promptFeedback.blockReason!=null` | `content_filter` |
| status=408/504 | `upstream_timeout` |
| status>=500 | `upstream_5xx` |

#### 4.3.5 `deepseek`（保持现状，不强制接入 normalizer）

DeepSeek 通道是单 provider，没有聚合负载需求，本期不接入。后期如果接其他 OpenAI/Anthropic 兼容上游做镜像负载再补。

### 4.4 200 + body 错的检测策略

- **不必每次解析 body**：只在 `Content-Type=application/json` 且 capability ∈ {chat, messages, gemini, images} 时尝试解析。
- **失败安全**：normalizer 解析 body 失败时一律返回 `nil`（视为成功），避免 normalizer 自己变成新的故障点。
- **流式响应**：SSE 已经走 `aggregateSSEResponse` 聚合后再判定，与非流式一致。

---

## 5. 对外错误格式（按 capability 组锁定）

### 5.1 OpenAI 协议组（`/chat/`、`/images/`、`/audio/`、`/embeddings/`、`/copilot/`）

```json
{
  "error": {
    "message": "<canonical message，中英文均可，与上游原文相近>",
    "type": "<由 canonical_reason 映射>",
    "code": "<canonical_reason>",
    "param": null
  }
}
```

`type` 映射：

| canonical_reason | type |
|---|---|
| `upstream_overloaded` / `upstream_5xx` / `upstream_timeout` / `upstream_network` / `unknown` | `server_error` |
| `upstream_rate_limited` / `quota_exceeded` | `rate_limit_error` |
| `invalid_request` / `model_not_found` | `invalid_request_error` |
| `content_filter` | `content_policy_violation` |
| `unauthorized` | `authentication_error` |

### 5.2 Anthropic 协议组（`/messages/`）

```json
{
  "type": "error",
  "error": {
    "type": "<由 canonical_reason 映射>",
    "message": "<canonical message>"
  }
}
```

`type` 映射：

| canonical_reason | type |
|---|---|
| `upstream_overloaded` | `overloaded_error` |
| `upstream_5xx` / `upstream_timeout` / `upstream_network` / `unknown` | `api_error` |
| `upstream_rate_limited` / `quota_exceeded` | `rate_limit_error` |
| `invalid_request` / `model_not_found` | `invalid_request_error` |
| `content_filter` | `invalid_request_error`（Anthropic 无独立内容审核错误，借用） |
| `unauthorized` | `authentication_error` |

### 5.3 Google 协议组（`/gemini/`）

```json
{
  "error": {
    "code": <HTTP status>,
    "message": "<canonical message>",
    "status": "<由 canonical_reason 映射，全大写>"
  }
}
```

`status` 映射：

| canonical_reason | status |
|---|---|
| `upstream_overloaded` | `RESOURCE_EXHAUSTED` |
| `upstream_5xx` / `upstream_timeout` / `upstream_network` / `unknown` | `INTERNAL` |
| `upstream_rate_limited` / `quota_exceeded` | `RESOURCE_EXHAUSTED` |
| `invalid_request` / `model_not_found` | `INVALID_ARGUMENT` |
| `content_filter` | `FAILED_PRECONDITION` |
| `unauthorized` | `UNAUTHENTICATED` |

### 5.4 HTTP status 选择

返回给客户端的 HTTP status 由 canonical_reason 映射，而非透传上游：

| canonical_reason | HTTP status |
|---|---|
| `upstream_overloaded` | 503 |
| `upstream_5xx` / `upstream_network` / `unknown` | 502 |
| `upstream_timeout` | 504 |
| `upstream_rate_limited` / `quota_exceeded` | 429 |
| `invalid_request` / `model_not_found` | 400 |
| `content_filter` | 400（OpenAI 历史习惯）/ 200 + 错误结构（如果客户端要求） |
| `unauthorized` | 401 |

---

## 6. Fallback 决策（聚合负载）

### 6.1 决策伪代码

```
attempted = {}
for attempt in 1..max_attempts:
    target = selectTarget(capability_pool, attempted)
    if target == nil:
        return final_error  // normalizer 输出的最后一个错误，按 capability 组协议渲染

    resp = forward(target, request)
    norm = normalize(target.endpoint_type, resp)

    if norm == nil:
        return resp  // 成功

    record_to_fallback_chain(target, norm)
    attempted.add(target.name)

    if norm.business_error:
        return render(norm)  // 业务错不 fallback，立即返回

    if not norm.retriable:
        return render(norm)

    target.MarkFailure(quietPeriod)
    continue  // 容量/超时/网络错，换 target 重试

return render(last_norm)  // 池内全打过一遍仍失败
```

### 6.2 max_attempts

- 默认：`min(3, len(capability_pool))`
- 单 target 池退化为 1 次（即不 fallback）
- 上限保留 3 是为了避免风暴：即便池里有 10 个 target，最多打 3 个就停

### 6.3 mute 时长

继承现有 `quietPeriod`（默认 60s）。本期不动。

---

## 7. 可观测性

### 7.1 Response Header

| Header | 含义 |
|---|---|
| `X-Proxy-Target` | 最终成功（或最后一次失败）的 target name |
| `X-Proxy-Capability` | 命中的 capability 前缀 |
| `X-Proxy-Fallback-Chain` | 失败链路：`target1:overloaded;target2:timeout`（仅在 attempt>1 时出现） |
| `X-Proxy-Canonical-Reason` | 最终错误的 canonical_reason（仅错误响应） |

### 7.2 结构化日志

每次 fallback 决策点写一条 INFO/WARN，字段：

```
{
  "request_id": "...",
  "capability": "chat",
  "model": "gpt-5.4",
  "attempt": 2,
  "target": "East US 2",
  "endpoint_type": "azure_openai",
  "upstream_status": 429,
  "canonical_reason": "upstream_overloaded",
  "retriable": true,
  "next_action": "fallback"
}
```

最终请求结束写一条 INFO，字段加 `total_attempts`、`final_target`、`final_status`、`duration_ms`。

---

## 8. 与现有机制的对接

### 8.1 复用项

| 现有机制 | 复用方式 |
|---|---|
| `selectTarget` | 不动；新增 `CapabilityHintFromContext` 限制 endpoint_type 池 |
| `targetState.MarkFailure / MarkSuccess` | 不动 |
| `affinity` 粘连 | 不动；capability 限制后粘连仍生效 |
| `path_capability.go` | 退化为 capability 内部的二级过滤（如 `/images/` 内 wangsu_openai_image 仍然只接 `/images/generations`） |
| `aggregateSSEResponse` | 不动；normalizer 在聚合后判定 |
| `EndpointTypeHint`（deepseek 用） | 保留；与 CapabilityHint 互斥共存（capability 内的 type 池由 capability 决定，不需要 endpoint_type hint） |

### 8.2 新增项

| 新增 | 位置（建议） |
|---|---|
| `CapabilityHint` context 注入 | `internal/proxy/capability_hint.go`（新文件） |
| Capability 分组表 | `internal/proxy/capability_registry.go`（新文件） |
| Normalizer 接口 + 各 endpoint_type 实现 | `internal/proxy/normalizer/`（新包） |
| Fallback 决策器 | 改造 `service.go::ServeHTTP` 主循环 |
| 对外错误渲染器 | `internal/proxy/error_render.go`（新文件） |
| capability 路由注册 | `cmd/proxy/main.go` 新增 6 段 `protected.HandleFunc("/{capability}/*")` |

### 8.3 移除项

| 移除 | 时机 |
|---|---|
| catch-all `protected.NotFound(proxyService.ServeHTTP)` | **本期不移除**；保留至 MeshyAI 等所有客户端迁移完成后再下架。新前缀路由就位后，catch-all 仍可工作，作为兼容期降级路径，但响应头加 `X-Proxy-Deprecated: catch-all-route, please migrate to /{capability}/` |

---

## 9. 客户端迁移影响

### 9.1 MeshyAI 侧需要改的 base_url

| Provider | 现 base_url | 新 base_url |
|---|---|---|
| OpenAI Chat | `http://192.168.33.110:8000/v1/` | `http://192.168.33.110:8000/chat/v1/` |
| OpenAI Image (gpt-image-2 等) | `http://192.168.33.110:8000/v1/` | `http://192.168.33.110:8000/images/v1/` |
| Claude | `http://192.168.33.110:8000/` | `http://192.168.33.110:8000/messages/` |
| Gemini | `http://192.168.33.110:8000/` | `http://192.168.33.110:8000/gemini/` |
| DeepSeek | `http://192.168.33.110:8000/deepseek/` | 不变 |

### 9.2 SDK 错误处理

按 capability 组锁定协议后，客户端 SDK 不需要改解析逻辑。但有两点需要文档化：
1. `code` / `error.code` 字段值现在是 llms-proxy canonical_reason，不是上游原始 code；
2. content_filter 在 OpenAI 组返回 400 + `error.code=content_policy_violation`，**不再**返回 200 + `finish_reason=content_filter`（这是与上游 OpenAI 的不一致点，需在 SDK 层面统一处理或在迁移文档中明示）。

> **决策点（待确认）**：content_filter 是否要保留 200 + finish_reason 的形态以兼容 OpenAI SDK？详见 §10 评审议题 Q2。

---

## 10. 评审议题（待用户拍板）

| 编号 | 议题 | 选项 |
|---|---|---|
| Q1 | catch-all 兼容期长度 | A. 即刻下架 / B. 保留 1 个月 / C. 保留至 MeshyAI 全部迁移完 |
| Q2 | content_filter 是否保留 OpenAI 的 200 + finish_reason 形态 | A. 严格按 §5 渲染为 400 / B. OpenAI 组保留 200 + finish_reason，其他组按表渲染 |
| Q3 | Gemini 的 RESOURCE_EXHAUSTED 是否区分配额限流与容量过载 | A. 一律按 overloaded 处理（fallback） / B. 解析 message 文本细分（脆弱） |
| Q4 | 网宿网关层伪 200 是否一律视为 5xx | A. 一律视为 upstream_5xx / B. 仅当 body 不含合法 JSON 时视为 / C. 透传不归一 |
| Q5 | normalizer 解析失败的兜底策略 | A. 视为成功（最宽松） / B. 视为 unknown（保守 fallback） / C. 看 status 二次判定 |
| Q6 | 是否在 capability 路由层加客户端级 ACL（principal.AllowAll() 之外，按 capability 限制） | A. 本期不做 / B. 同步实现 |
| Q7 | `/audio/`、`/embeddings/` 这种当前只有 1 个 target 的前缀是否值得现在就建 | A. 一并建（一致性） / B. 等真有第 2 个 target 再建 |

---

## 11. 实施分期

### Phase 0：本设计文档评审（当前）

输出：本文档定稿，Q1–Q7 全部拍板。

### Phase 1：基础设施

- 新增 `capability_hint.go`、`capability_registry.go`
- 新增 normalizer 接口与 5 个 endpoint_type 实现（azure_openai、wangsu_openai 系、wangsu_claude、wangsu_gemini）
- 新增 error_render.go（3 套协议渲染器）
- 单元测试覆盖：每个 normalizer 至少 8 个 case（覆盖 §4.3 全部规则行）
- **不接入路由**，单元测试通过即合并

### Phase 2：路由接入（capability 前缀 + fallback 决策）

- `cmd/proxy/main.go` 注册 6 个 capability 前缀
- 改造 `service.go::ServeHTTP` 主循环为 §6.1 决策伪代码
- catch-all 加 `X-Proxy-Deprecated` 标记，行为不变
- 端到端测试：每个 capability 至少 1 个成功 + 1 个 fallback + 1 个业务错 case
- 33.110 灰度部署，观察 1 周

### Phase 3：客户端迁移

- MeshyAI 各 Provider 改 base_url（§9.1）
- 验证 33.110 access log 中 catch-all 流量降为 0（除少量遗留外部客户端）

### Phase 4：catch-all 下架

- 移除 `protected.NotFound(proxyService.ServeHTTP)`
- catch-all 改为返回 404 + 引导到对应 capability 前缀的错误响应

---

## 12. 风险与缓解

| 风险 | 缓解 |
|---|---|
| normalizer 漏判某种上游错误形态，导致业务错被 fallback / 容量错没 fallback | Phase 1 单测覆盖；Phase 2 灰度期看 `X-Proxy-Canonical-Reason=unknown` 比例，>1% 触发回顾 |
| 对外错误格式与上游 SDK 期望不一致 | §9.2 文档化；Q2 评审议题专门讨论 |
| capability 池内 target 同模型存在但参数能力差异（如 Azure 不支持某 OpenAI 参数） | 现有 `sanitizeRequestBodyForAzure` 复用；新增 capability 不放大此问题 |
| catch-all 与 capability 路由共存期，某些客户端继续走 catch-all 拿不到新错误格式 | 兼容期内 catch-all 透传上游原始错误（保持现行行为），不做 normalizer 渲染；只在 capability 路由生效 |
| Phase 2 灰度发现某 normalizer 规则错误 | normalizer 在独立包，可独立回滚；fallback 决策器有 feature flag（环境变量 `PROXY_DISABLE_NORMALIZER=1` 退化为透传） |

---

## 13. 验收标准

Phase 1 验收：
- 全部 normalizer 单测通过；
- `go vet`、`go test ./...`、`golangci-lint run` 全绿。

Phase 2 验收：
- 6 个 capability 前缀均可工作；
- 端到端测试覆盖 §4.3 主要 case；
- 灰度 1 周内 `X-Proxy-Canonical-Reason=unknown` 比例 < 1%；
- 灰度期内未出现"业务错被 fallback"或"容量错没 fallback"案例（看 access log）。

Phase 3 验收：
- MeshyAI 全部 Provider 迁移完毕；
- 33.110 catch-all 流量 < 5%。

Phase 4 验收：
- catch-all 移除后 1 周内无外部客户端报错。

---

## 附录 A：当前 33.110 target 与 capability 映射对照

| Target name | endpoint_type | capability |
|---|---|---|
| OpenAI-Chat | wangsu_openai | `/chat/` |
| East US 2 | azure_openai | `/chat/`、`/embeddings/`、`/images/`（按 model 决定） |
| North Central US | azure_openai | `/audio/`（tts/whisper） |
| OpenAI-Image-Gen | wangsu_openai_image | `/images/` |
| OpenAI-Image-Edits | wangsu_openai_image_edit | `/images/` |
| Claude | wangsu_claude | `/messages/` |
| Gemini | wangsu_gemini | `/gemini/` |
| DeepSeek | deepseek | `/deepseek/`（独立） |

> 注：East US 2 同时归属 `/chat/`、`/embeddings/`、`/images/` 三个 capability。target 与 capability 是多对多关系，由 endpoint_type + allowed_models + path 共同决定具体落在哪个 capability 池里。selectTarget 在指定 capability 池内时按 §3.3 路径白名单 + 现有 modelAllowed + path_capability 三层过滤。
