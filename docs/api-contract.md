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

### DeepSeek dual-format sub-route (`/deepseek/**`)

DeepSeek officially exposes the same models behind two parallel API surfaces: an OpenAI-compatible base (`https://api.deepseek.com`) and an Anthropic-compatible base (`https://api.deepseek.com/anthropic`). Both surfaces accept the same Bearer API key. To avoid forcing operators to register two near-identical targets, the proxy mounts a single sub-route that auto-detects the format from the request path.

- **Mount point**: `/deepseek/*` (authenticated, behind the same bearer-token middleware as other client routes).
- **Routing constraint**: requests under `/deepseek/*` are pinned to targets whose `endpoint_type` is `deepseek` — the standard `X-Proxy-Target` / `?target=` hint and `allowed_targets` rules still apply, but only `deepseek` targets are eligible regardless of the request path.
- **Path stripping**: the `/deepseek` prefix is removed before forwarding. The remaining path is then routed to the appropriate upstream surface:
  - Paths matching `/v1/messages` (with or without trailing segments) are treated as Anthropic-format calls; the proxy prepends `/anthropic` to the upstream path. Example: client `POST /deepseek/v1/messages` → upstream `POST https://api.deepseek.com/anthropic/v1/messages`.
  - All other paths (e.g. `/chat/completions`, `/v1/chat/completions`, `/embeddings`) are forwarded as-is to the OpenAI-compatible surface. Example: client `POST /deepseek/v1/chat/completions` → upstream `POST https://api.deepseek.com/v1/chat/completions`.
- **Authentication**: both formats receive `Authorization: Bearer <key>` injected from the target's `api_key`. Unlike upstream Anthropic, DeepSeek's Anthropic-compatible surface does **not** use `x-api-key`.
- **Client SDK configuration**:
  - OpenAI SDK: set `base_url` to `https://<your-domain>/deepseek` (or `/deepseek/v1`, depending on the SDK's path conventions) and `api_key` to a proxy bearer token.
  - Anthropic SDK: set `base_url` to `https://<your-domain>/deepseek` and `api_key` to a proxy bearer token; the SDK will issue `POST /v1/messages` under that base.
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
