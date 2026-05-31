# API Contract & Error Codes

This document describes the externally visible HTTP contract that the proxy exposes to internal clients and operators. The proxy supports multiple upstream endpoint types: **Azure OpenAI**, **OpenAI**, **Claude (Anthropic)**, **Gemini (Google)**, **Wangsu OpenAI / Claude / Gemini**, **Wangsu OpenAI Image / Image Edit**, **GitHub Copilot**, and **DeepSeek**. All endpoints speak JSON over HTTP/1.1. Unless noted otherwise, responses adopt UTF-8 encoding.

## Authentication
- Client requests **must** include `Authorization: Bearer <access-key>`.
- Also supports Azure-style client auth: `api-key: <access-key>` (header) or `?api-key=<access-key>` (query).
- Tokens map 1:1 to entries in the bbolt database (managed via `/admin/data/clients`).
- On authentication failure the proxy returns `401 Unauthorized` and a `WWW-Authenticate: Bearer` header.
- Requests receive an `X-Request-ID` header (generated when missing) that propagates through logs.

## Public Endpoints

### `GET /healthz`
- No authentication required.
- Returns static readiness information:
  ```json
  {"status":"ok"}
  ```
- Used by load balancers or container probes to confirm the process is alive.

## Authenticated Client Endpoints

### `GET /api/ping`
- Diagnostic endpoint for clients to verify credentials.
- Response:
  ```json
  {"message":"pong","client":"<client-name>"}
  ```

### Proxy Pass-through (`/**`)
- Any other path mounted beneath `/` forwards to the configured upstream (Azure OpenAI, OpenAI, Claude, Gemini, or Wangsu variants).
- Clients should call the proxy with OpenAI-style paths (for example `/v1/chat/completions`, `/v1/embeddings`, `/v1/images/generations`).
- The upstream target is selected automatically; clients may opt-in to a specific backend by:
  - Header: `X-Proxy-Target: <target-name>`
  - Query: `?target=<target-name>`
- Model-aware routing is enforced when `allowed_models` is configured on targets:
  - Requests with a `model` outside all allowed target lists return `400 Bad Request`.
  - Supported extraction sources: JSON body, `application/x-www-form-urlencoded`, and `multipart/form-data`.
- The proxy rewrites the `Host` header to the upstream endpoint.
- **Upstream authentication differs by target `endpoint_type`:**
  - `azure_openai` — injects `api-key: <key>` header (or forwards `X-Azure-Authorization` when `allow_bearer_passthrough` is enabled).
  - `openai` — injects `Authorization: Bearer <key>`.
  - `claude` — injects `x-api-key: <key>` and sets `anthropic-version: 2023-06-01` if not already present.
  - `gemini` — injects `x-goog-api-key: <key>`.
  - `wangsu_openai` — injects `Authorization: Bearer <key>` (same as `openai`).
  - `wangsu_claude` — injects `x-api-key: <key>` and sets `anthropic-version: 2023-06-01` (same as `claude`).
  - `wangsu_gemini` — injects `x-goog-api-key: <key>` (same as `gemini`).
  - `deepseek` — injects `Authorization: Bearer <key>` for both OpenAI- and Anthropic-compatible formats. See **DeepSeek dual-format sub-route** below.
- **Azure parameter whitelist filtering** (stripping unsupported request body fields) is applied **only** to `azure_openai` targets. Requests to `openai`, `claude`, `gemini`, `deepseek`, and Wangsu variant targets forward the original body unmodified.
- **Path compatibility**: Target selection checks path compatibility by `endpoint_type`. `wangsu_openai` only supports `/chat/completions`, `/images/generations`, and `/embeddings`; targets whose `endpoint_type` is incompatible with the request path are automatically skipped during selection. All other endpoint types accept any path.
- **Connection affinity**: Requests from the same client with the same model are preferentially routed to the same target, improving upstream token cache (KV cache / prompt cache) hit rates. Affinity entries have a TTL of 5 minutes with lazy expiration. When the affinity target is unavailable or path-incompatible, routing falls back to round-robin selection.
- The proxy removes internal/legacy query params before forwarding: `target`, `api-version`, `api_version`, `api-key`.
- Successful responses set **both** `X-Proxy-Target: <target-name>` and `X-Azure-Target: <target-name>` so callers can identify the chosen backend. (`X-Azure-Target` is retained for backward compatibility with older clients.)
- Streaming responses are relayed chunk-by-chunk (`io.Copy`), preserving status codes and headers except for hop-by-hop headers.

### DeepSeek dual-format (`deepseek`)

DeepSeek officially exposes the same models behind two parallel API surfaces: an OpenAI-compatible base (`https://api.deepseek.com`) and an Anthropic-compatible base (`https://api.deepseek.com/anthropic`). Both surfaces accept the same Bearer API key. The proxy auto-detects the format from the request path.

- **Access**: DeepSeek targets are accessed through the root path `/` like other native endpoint types. Use `X-Proxy-Target: <target-name>` or configure `allowed_targets` to route to DeepSeek targets.
- **Path handling**: the request path is forwarded directly; no prefix stripping is needed:
  - Paths matching `/v1/messages` (with or without trailing segments) are treated as Anthropic-format calls; the proxy prepends `/anthropic` to the upstream path. Example: client `POST /v1/messages` → upstream `POST https://api.deepseek.com/anthropic/v1/messages`.
  - All other paths (e.g. `/chat/completions`, `/v1/chat/completions`, `/embeddings`) are forwarded as-is to the OpenAI-compatible surface. Example: client `POST /v1/chat/completions` → upstream `POST https://api.deepseek.com/v1/chat/completions`.
- **Authentication**: both formats receive `Authorization: Bearer <key>` injected from the target's `api_key`. Unlike upstream Anthropic, DeepSeek's Anthropic-compatible surface does **not** use `x-api-key`.
- **Client SDK configuration**:
  - OpenAI SDK: set `base_url` to `https://<your-domain>/` (or `/v1`, depending on the SDK's path conventions) and `api_key` to a proxy bearer token; use `X-Proxy-Target: <deepseek-target>` to select the target.
  - Anthropic SDK: set `base_url` to `https://<your-domain>/` and `api_key` to a proxy bearer token; the SDK will issue `POST /v1/messages` under that base. Use `X-Proxy-Target: <deepseek-target>` to select the target.
- **Affinity & failover**: standard connection-affinity and multi-target failover behavior applies within the `deepseek` endpoint type — operators may register multiple `deepseek` targets (e.g. for multi-key pooling) and the proxy will round-robin / failover between them.

## Admin Authentication (Session-based)
The admin management system uses **independent username/password authentication**, completely separate from the client proxy bearer-token auth.

- Admin users are stored in the bbolt database (managed via admin API).
- Passwords are stored as `sha256$<salt>$<hex>` hashes.
- After login, the server sets a signed session cookie (`llms_proxy_admin_session` by default).
- Session configuration (cookie name, secret, TTL, sliding expiration) is defined in `config.admin_session`.
- Unauthenticated requests to `/admin/*` are redirected to `/login` with HTTP 302.

### `GET /login`
- Renders the admin login page (HTML form).
- No authentication required.

### `POST /login`
- Accepts `application/x-www-form-urlencoded` with `username` and `password` fields.
- On success: sets session cookie and redirects to `/admin` (HTTP 302).
- On failure: re-renders login page with error message.

### `POST /logout`
- Destroys the session and clears the cookie.
- Redirects to `/login` (HTTP 302).

## Admin API (Session-Protected)
All `/admin/*` endpoints require a valid session cookie (obtained via `/login`).

### `GET /admin/`
- Web management console entry point (HTML) — left-sidebar navigation with 6 pages: Overview, Clients, Target Management, Model Costs, Usage Statistics, Audit Logs.
- Single-file no-build UI embedded via `go:embed`, no external frontend dependencies.

### `GET /admin/api/me`
- Returns the currently authenticated admin user info:
  ```json
  {"authenticated":true,"username":"admin","role":"admin","expires_at":"2026-03-20T09:00:00Z"}
  ```

### `GET /admin/api/overview`
- Returns dashboard overview data including targets, request counters, usage summaries, and recent audit events.

### `GET /admin/api/audit`
- Returns audit log entries with optional pagination (`limit`, `offset`).

### `GET /admin/healthz`
- Returns current status for each configured target (across all endpoint types), including mute windows.
- Each target object now includes `endpoint_type`.
- Example payload:
  ```json
  {
    "status": "ok",
    "checked_at": "2024-05-10T12:00:00Z",
    "targets": [
      {
        "name": "east2",
        "endpoint_type": "azure_openai",
        "endpoint": "https://example.openai.azure.com",
        "muted": false,
        "last_success": "2024-05-10T11:59:50Z",
        "last_failure": "2024-05-10T11:20:34Z"
      }
    ],
    "target_count": 2,
    "muted_targets": 0
  }
  ```

### `GET /admin/metrics`
- Returns aggregate counters since process start:
  ```json
  {
    "generated_at": "2024-05-10T12:00:00Z",
    "uptime_seconds": 3600,
    "active_requests": 0,
    "requests": {
      "total": 1024,
      "success": 1000,
      "failures": 24,
      "retries": 12
    },
    "targets": 2
  }
  ```

### `POST /admin/config/reload`
- Reloads configuration from disk and applies it to both the proxy and auth store.
- No request body is required.
- On success returns:
  ```json
  {
    "status": "ok",
    "reloaded_at": "2024-05-10T12:00:05Z",
    "targets": 2,
    "clients": 5
  }
  ```
- If validation fails, the proxy preserves the previous configuration and returns an error response.

### `GET /admin/data/clients`
- Returns the current client list from the bbolt database.

### `POST /admin/data/clients`
- Creates a client in the bbolt database.

### `PUT /admin/data/clients/{name}`
- Updates the named client.

### `DELETE /admin/data/clients/{name}`
- Deletes the named client.

### `GET /admin/data/model-costs`
- Returns model token cost configuration from the bbolt database.
- Each record includes an `endpoint_type` field (defaults to `azure_openai` when not set).

### `PUT /admin/data/model-costs/{model}`
- Inserts or updates a model cost record.
- Request body accepts an `endpoint_type` field. If omitted, defaults to `azure_openai`.
- Example body:
  ```json
  {
    "endpoint_type": "openai",
    "input_per_1m_tokens": 5,
    "output_per_1m_tokens": 15,
    "cached_input_per_1m_tokens": 2.5
  }
  ```
- Response: `{"ok": true, "model": "<model>", "endpoint_type": "<type>"}`.

### `DELETE /admin/data/model-costs/{model}`
- Deletes a model cost record.
- Supports optional query parameter `?endpoint_type=<type>` for endpoint-type-aware deletion. Without the parameter, removes all records matching the model name (legacy behavior).
- Returns `204 No Content` on success.

### Target Management

### `GET /admin/data/targets`
- Returns all configured upstream targets.
- The `api_key` field is **never** returned; instead, each target includes `has_api_key: true|false`.
- Response example:
  ```json
  {
    "targets": [
      {
        "name": "my-openai",
        "endpoint_type": "openai",
        "endpoint": "https://api.openai.com",
        "resource_path_prefix": "",
        "has_api_key": true,
        "allow_bearer_passthrough": false,
        "allowed_models": ["gpt-4o"]
      }
    ],
    "count": 1
  }
  ```

### `POST /admin/data/targets`
- Creates a new upstream target. The new target is appended to `targets` in `config.json` and applied at runtime.
- Required fields: `name`, `endpoint`. Either `api_key` or `allow_bearer_passthrough: true` must be provided.
- `resource_path_prefix` is required only for `azure_openai` targets.
- `endpoint_type` defaults to `azure_openai` when omitted. Valid values: `azure_openai`, `openai`, `claude`, `gemini`, `wangsu_openai`, `wangsu_claude`, `wangsu_gemini`, `wangsu_openai_image`, `wangsu_openai_image_edit`, `copilot`, `deepseek`. The authoritative list is exposed at runtime via `GET /admin/data/endpoint-types`.
- Request body example:
  ```json
  {
    "name": "my-openai",
    "endpoint_type": "openai",
    "endpoint": "https://api.openai.com",
    "api_key": "sk-xxx",
    "allowed_models": ["gpt-4o"]
  }
  ```
- Response: `201 Created` with `{"ok": true, "name": "<name>"}`.
- Returns `409 Conflict` if a target with the same name already exists.

### `PUT /admin/data/targets/{name}`
- Updates an existing target identified by `{name}` (case-insensitive match).
- Only provided fields are updated. `api_key` may be set to `null` to leave it unchanged.
- Response: `{"ok": true, "name": "<name>"}`.
- Returns `404 Not Found` if the target does not exist.

### `DELETE /admin/data/targets/{name}`
- Removes a target from configuration and runtime.
- Response: `{"ok": true}`.
- Returns `404 Not Found` if the target does not exist.

### Model Catalog

### `GET /admin/data/catalog`
- Returns the embedded model metadata catalog.
- Supports optional query parameter `?endpoint_type=<type>` to filter by endpoint type (e.g., `openai`, `claude`, `azure_openai`).
- Without the parameter, returns all models across all endpoint types.
- Response:
  ```json
  {
    "models": [
      {
        "endpoint_type": "openai",
        "model": "gpt-4o",
        "display_name": "GPT-4o",
        "aliases": ["gpt-4o-2024-05-13"],
        "capabilities": ["chat", "vision"],
        "default_cost": {
          "input_per_1m_tokens": 5,
          "output_per_1m_tokens": 15,
          "cached_input_per_1m_tokens": 2.5
        }
      }
    ],
    "count": 1
  }
  ```

### `GET /admin/data/catalog/{endpoint_type}`
- Returns catalog entries filtered to the specified `endpoint_type` (path parameter).
- Response format is identical to `GET /admin/data/catalog`.

### `GET /admin/data/usage/events`
- Returns usage events from the bbolt database with optional filters (`from`, `to`, `client_name`, `model`, `limit`).

### `GET /admin/data/usage/aggregate`
- Returns aggregated token/cost data by hour or day.

### `GET /admin/data/usage/summary`
- Returns fixed windows for `last_hour`, `yesterday`, and `last_30_days`.

## Error Codes

| Status | Scenario | Notes |
| ------ | -------- | ----- |
| `400 Bad Request` | Malformed client request or unknown `target` value. | Returned before contacting upstream. |
| `401 Unauthorized` | Missing/invalid bearer token. | Includes `WWW-Authenticate: Bearer`. |
| `403 Forbidden` | Authenticated client requested a disallowed target. | Token `allowed_targets` restriction triggered. |
| `404 Not Found` | Admin router fallback or upstream returned 404. | Proxy passes upstream 404 as-is. |
| `429 Too Many Requests` | Propagated from upstream. | Proxy does not alter upstream rate-limit semantics. |
| `500 Internal Server Error` | Unhandled panic recovered by middleware. | Logged with `request_id`. |
| `502 Bad Gateway` | All targets unavailable or upstream returned 5xx. | `X-Proxy-Target` / `X-Azure-Target` still indicates the final attempt. |
| `503 Service Unavailable` | Requested target muted/unavailable during selection. | Client can retry later or omit explicit target. |
| `504 Gateway Timeout` | Transport timeout while contacting upstream. | Triggered by request timeout or network errors. |

The proxy increments internal metrics for total requests, retries, successes, and failures; these counters are exposed via `/admin/metrics`.

## Headers of Interest
- `Authorization: Bearer <token>` (required for authenticated routes)
- `X-Proxy-Target` / `target` query (optional target hint on request; also set as a **response** header identifying the chosen backend)
- `X-Azure-Target` (response header showing chosen backend — retained for backward compatibility; identical value to `X-Proxy-Target`)
- `X-Request-ID` (included in every response and log entry)
- `WWW-Authenticate: Bearer` (sent on `401 Unauthorized`)

---

## 路由架构：原厂API vs 非原厂API

代理将所有上游目标分为两类：

| 分类 | 路径入口 | 说明 | 包含的 endpoint_type |
|------|---------|------|---------------------|
| **原厂API** | 根路径 `/`（catch-all） | 上游提供标准厂商原生 API，代理仅做认证适配和转发 | `azure_openai`, `openai`, `claude`, `gemini`, `deepseek`, `bailian`, `wangsu_openai`, `wangsu_claude`, `wangsu_gemini`, `wangsu_openai_image`, `wangsu_openai_image_edit` |
| **非原厂API** | 专用路径前缀 | 上游协议与标准厂商 API 有显著差异，需要专用处理链 | `copilot`（`/copilot/*`） |

### 原厂API 路由
客户端请求进入根路径 catch-all（`ServeHTTP`），代理按以下流程处理：
1. 鉴权 → 提取模型名 → 按 `endpoint_type` 过滤目标池
2. 路径兼容性检查（`PathSupportedByEndpointType`）
3. 连接粘连（affinity）或轮询选择目标
4. 按 `endpoint_type` 注入认证头
5. 转发请求、回写响应

### 非原厂API 路由
- **Copilot**：`/copilot/*` 路径 → `HandleCopilotPassthrough`（OAuth Token 池、模型名映射、premium request 计费）

---

## 原厂API 详细格式

### OpenAI 兼容（`openai`）

**上游认证**：`Authorization: Bearer <api-key>`

**客户端调用格式**：标准 OpenAI API，base_url 指向代理。

```bash
# Chat Completions
curl <proxy-host>/v1/chat/completions \
  -H "Authorization: Bearer <proxy-token>" \
  -H "Content-Type: application/json" \
  -H "X-Proxy-Target: <target-name>" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}'
```

**适用目标**：OpenAI 官方、任何 OpenAI 兼容第三方（如 Token Plan OpenAI 兼容端点）。

### Claude / Anthropic（`claude`）

**上游认证**：
- 默认：`x-api-key: <api-key>` + `anthropic-version: 2023-06-01`
- `auth_mode: "bearer"` 时：`Authorization: Bearer <api-key>` + `anthropic-version: 2023-06-01`

**客户端调用格式**：标准 Anthropic API。

```bash
# Messages API
curl <proxy-host>/v1/messages \
  -H "Authorization: Bearer <proxy-token>" \
  -H "Content-Type: application/json" \
  -H "X-Proxy-Target: <target-name>" \
  -d '{"model":"claude-sonnet-4-20250514","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'
```

**`auth_mode` 配置**：
- 留空或 `"x-api-key"`（默认）：使用 `x-api-key` 头 — 适用于 Anthropic 官方、网宿 Claude 通道
- `"bearer"`：使用 `Authorization: Bearer` 头 — 适用于 Token Plan Anthropic 兼容端点等非标准上游

### Gemini（`gemini`）

**上游认证**：`x-goog-api-key: <api-key>`

**客户端调用格式**：标准 Gemini API。

```bash
curl "<proxy-host>/v1beta/models/gemini-2.5-pro:generateContent" \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: <target-name>" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"parts":[{"text":"Hello"}]}]}'
```

### Azure OpenAI（`azure_openai`）

**上游认证**：`api-key: <api-key>` 或 Bearer 透传（当 `allow_bearer_passthrough: true` 且客户端发送 `X-Azure-Authorization` 头时）。

**客户端调用格式**：Azure OpenAI 路径格式，需配置 `resource_path_prefix`。

```bash
curl <proxy-host>/openai/deployments/gpt-4o/chat/completions?api-version=2025-04-01-preview \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: <target-name>" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Hello"}]}'
```

**注意**：代理会自动追加 `api-version` 查询参数（对含 `/deployments/` 的路径）。客户端无需传递。

### 网宿 OpenAI 兼容（`wangsu_openai`）

**上游认证**：`Authorization: Bearer <api-key>`（同 `openai`）

**路径限制**：仅支持 `/chat/completions`、`/images/generations`、`/images/edits`、`/images/variations`、`/embeddings`。不兼容路径的目标在目标选择时自动跳过。

### 网宿图像通道（`wangsu_openai_image` / `wangsu_openai_image_edit`）

**上游认证**：`Authorization: Bearer <api-key>`

**特殊行为**：`endpoint` 配置为终态 URL，客户端请求路径被完全覆盖（不拼接）。客户端按 OpenAI 官方路径调用：

```bash
# 文生图（wangsu_openai_image）
curl <proxy-host>/v1/images/generations \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: <target-name>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-image-1","prompt":"a cat","size":"1024x1024"}'

# 图编辑（wangsu_openai_image_edit）
curl <proxy-host>/v1/images/edits \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: <target-name>" \
  -F "model=gpt-image-1" \
  -F "image=@photo.png" \
  -F "prompt=add sunglasses"
```

---

## 非原厂API 详细格式

### Copilot（`/copilot/*`）

**入口**：必须通过 `/copilot/*` 路径访问，不支持根路径模型名前缀拦截。

**上游认证**：动态 OAuth Token（由 Copilot 账户池管理），无需配置 `api_key`。

**子路由**：

| 路径 | 方法 | 说明 |
|------|------|------|
| `/copilot/auth` | GET | 检查 Copilot 池可用性 |
| `/copilot/quota` | GET | 查看 premium request 配额 |
| `/copilot/models` | GET | 透传上游模型列表 |
| `/copilot/*` | * | 透明代理（剥 `/copilot` 前缀后转发） |

**客户端调用格式**：OpenAI 兼容（Copilot 上游统一使用 `/chat/completions`）。

```bash
curl <proxy-host>/copilot/v1/chat/completions \
  -H "Authorization: Bearer <proxy-token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4.6","messages":[{"role":"user","content":"Hello"}]}'
```

**X-Initiator 头**：代理自动推断（`user` 扣 premium request，`agent` 不扣）。客户端可显式设置 `X-Initiator: user|agent` 覆盖推断。

### DeepSeek (`deepseek`)

**入口**：通过根路径 `/` 访问，与其他原厂模型一致。使用 `X-Proxy-Target: <target-name>` 或 `allowed_targets` 选择 DeepSeek 目标。

**上游认证**：`Authorization: Bearer <api-key>`（OpenAI 和 Anthropic 两种格式统一使用 Bearer）。

**双协议支持**：
- 路径含 `/v1/messages*` → 上游自动加 `/anthropic` 前缀（Anthropic 格式）
- 其他路径 → 直通（OpenAI 格式）

```bash
# OpenAI 兼容格式
curl <proxy-host>/v1/chat/completions \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: <deepseek-target>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"Hello"}]}'

# Anthropic 兼容格式
curl <proxy-host>/v1/messages \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: <deepseek-target>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'
```

**多 target 支持**：可在 `endpoint_type=deepseek` 下注册多个 target（多 key 池/容灾），标准 affinity + failover 行为生效。

---

## Token Plan 接入方案

阿里云百炼 Token Plan 团队版提供三个 API 端点，可通过本代理统一接入。

### 端点信息

| 协议 | Base URL | 认证方式 |
|------|----------|---------|
| OpenAI 兼容 | `https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1` | `Authorization: Bearer sk-sp-xxx` |
| Anthropic 兼容 | `https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic` | `Authorization: Bearer sk-sp-xxx`（注意：非 x-api-key） |
| 图像生成（DashScope 原生） | `https://token-plan.cn-beijing.maas.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation` | `Authorization: Bearer sk-sp-xxx` |

### 代理配置示例

使用单一 `bailian` 类型，按客户端请求路径自动分流到上游 OpenAI 或 Anthropic 兼容端点。

```json
{
  "name": "token-plan",
  "endpoint_type": "bailian",
  "endpoint": "https://token-plan.cn-beijing.maas.aliyuncs.com",
  "api_key": "sk-sp-xxx",
  "allowed_models": ["qwen3.7-max", "qwen3.6-plus", "qwen3.6-flash", "deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v3.2", "kimi-k2.6", "kimi-k2.5", "glm-5.1", "glm-5", "MiniMax-M2.5"]
}
```

客户端调用 OpenAI 格式（自动路由到 `/compatible-mode/v1`）：
```bash
curl <proxy-host>/v1/chat/completions \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: token-plan" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3.7-max","messages":[{"role":"user","content":"你好"}]}'
```

客户端调用 Anthropic 格式（自动路由到 `/apps/anthropic/v1/messages`）：
```bash
curl <proxy-host>/v1/messages \
  -H "Authorization: Bearer <proxy-token>" \
  -H "X-Proxy-Target: token-plan" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3.7-max","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

#### 图像生成端点（暂不接入）

Token Plan 图像生成使用 DashScope 原生 API 格式（非 OpenAI/Claude/Gemini 标准），需要专用处理链。当前代理不支持此格式，需后续开发专用 endpoint_type 或通过网宿图像通道中转。

### Token Plan 支持模型列表

| 厂商 | 模型 | OpenAI 兼容 | Anthropic 兼容 |
|------|------|:-----------:|:--------------:|
| 通义千问 | qwen3.7-max | ✅ | ✅ |
| 通义千问 | qwen3.6-plus | ✅ | ✅ |
| 通义千问 | qwen3.6-flash | ✅ | ✅ |
| 通义千问 | qwen-image-2.0 | ✅ | ❌ |
| 通义千问 | qwen-image-2.0-pro | ✅ | ❌ |
| 万象 | wan2.7-image | ✅ | ❌ |
| 万象 | wan2.7-image-pro | ✅ | ❌ |
| DeepSeek | deepseek-v4-pro | ✅ | ✅ |
| DeepSeek | deepseek-v4-flash | ✅ | ✅ |
| DeepSeek | deepseek-v3.2 | ✅ | ✅ |
| 月之暗面 | kimi-k2.6 | ✅ | ❌ |
| 月之暗面 | kimi-k2.5 | ✅ | ❌ |
| 智谱 | glm-5.1 | ✅ | ✅ |
| 智谱 | glm-5 | ✅ | ✅ |
| MiniMax | MiniMax-M2.5 | ✅ | ❌ |

### API Key 说明
- Token Plan API Key 格式：`sk-sp-xxx`（区别于普通百炼 `sk-xxx` 和 Coding Plan key）
- 由管理员在百炼控制台生成
- 三套 key 体系互不兼容：Token Plan / Coding Plan / 百炼按量付费

---

## 请求测试清单

| # | 场景 | 方法 | 预期 |
|---|------|------|------|
| 1 | OpenAI 目标 chat completions | `POST /v1/chat/completions` + `X-Proxy-Target: openai-target` | 200 + 流式/JSON 响应 |
| 2 | Claude 目标 messages | `POST /v1/messages` + `X-Proxy-Target: claude-target` | 200 + Anthropic 格式响应 |
| 3 | 百炼 Anthropic 格式 | `POST /v1/messages` + `X-Proxy-Target: token-plan` | 200 + 上游路由到 `/apps/anthropic`，Bearer 认证 |
| 4 | Gemini 目标 generateContent | `POST /v1beta/models/gemini-2.5-pro:generateContent` | 200 + Gemini 格式响应 |
| 5 | Azure 目标 deployments 路径 | `POST /openai/deployments/gpt-4o/chat/completions` | 200 + api-version 自动追加 |
| 6 | 网宿图像文生图 | `POST /v1/images/generations` + `X-Proxy-Target: wangsu-image` | 200 + 图片 URL/b64 |
| 7 | Copilot 透传 | `POST /copilot/v1/chat/completions` | 200 + OAuth token 动态注入 |
| 8 | DeepSeek OpenAI 格式 | `POST /v1/chat/completions` + `X-Proxy-Target: deepseek-target` | 200 + 直通上游 |
| 9 | DeepSeek Anthropic 格式 | `POST /v1/messages` + `X-Proxy-Target: deepseek-target` | 200 + 上游加 `/anthropic` 前缀 |
| 10 | 百炼 OpenAI 格式 | `POST /v1/chat/completions` + `X-Proxy-Target: token-plan` | 200 + 上游路由到 `/compatible-mode`，qwen/deepseek 等模型 |

---

## 百炼 API 双协议接入方案

阿里云百炼 API 公开端点可使用 `endpoint_type=bailian_api` 作为单一 target 接入，避免为同一个百炼 API Key 分别创建 OpenAI/Claude 两个同名 target。

### 端点信息

| 协议 | Base URL | 代理路由规则 |
|------|----------|--------------|
| OpenAI Chat/Embeddings 兼容 | `https://dashscope.aliyuncs.com/compatible-mode/v1` | 客户端 `/v1/chat/completions`、`/v1/embeddings` 等转发到 `/compatible-mode/v1/*` |
| OpenAI Responses 兼容 | `https://dashscope.aliyuncs.com/api/v2/apps/protocols/compatible-mode/v1` | 客户端 `/v1/responses*` 转发到 `/api/v2/apps/protocols/compatible-mode/v1/responses*` |
| Anthropic 兼容 | `https://dashscope.aliyuncs.com/apps/anthropic` | 客户端 `/v1/messages*` 转发到 `/apps/anthropic/v1/messages*` |

### 代理配置示例

```json
{
  "name": "bailian-api",
  "endpoint_type": "bailian_api",
  "endpoint": "https://dashscope.aliyuncs.com",
  "api_key": "sk-xxx",
  "allowed_models": ["qwen-plus", "qwen-max"]
}
```

`endpoint` 建议填写区域根 URL。若粘贴了官方 OpenAI/Responses/Anthropic base URL，代理会在 `bailian_api` 模式下去除已知协议前缀后再按客户端路径重新分流。
| 11 | 模型白名单拒绝 | 请求不在 `allowed_models` 中的模型 | 403 Forbidden |
| 12 | 路径不兼容跳过 | `POST /v1/images/generations` 但目标为 `wangsu_openai`（支持）vs 其他类型 | 自动选择兼容目标 |
| 13 | 目标 failover | 主目标网络不可达 | 自动切换到备用目标 |
| 14 | 连接粘连 | 同客户端 + 同模型连续请求 | 倾向路由到同一目标 |
