# API Contract & Error Codes

This document describes the externally visible HTTP contract that the Azure OpenAI proxy exposes to internal clients and operators. All endpoints speak JSON over HTTP/1.1. Unless noted otherwise, responses adopt UTF-8 encoding.

## Authentication
- Client requests **must** include `Authorization: Bearer <access-key>`.
- Also supports Azure-style client auth: `api-key: <access-key>` (header) or `?api-key=<access-key>` (query).
- Tokens map 1:1 to entries in `./config/clients.json` (configured via `config.data_files.clients_file`).
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
- Any other path mounted beneath `/` forwards to Azure OpenAI.
- Clients should call the proxy with OpenAI-style paths (for example `/v1/chat/completions`, `/v1/embeddings`, `/v1/images/generations`).
- The upstream target is selected automatically; clients may opt-in to a specific backend by:
  - Header: `X-Proxy-Target: <target-name>`
  - Query: `?target=<target-name>`
- Model-aware routing is enforced when `allowed_models` is configured on targets:
  - Requests with a `model` outside all allowed target lists return `400 Bad Request`.
  - Supported extraction sources: JSON body, `application/x-www-form-urlencoded`, and `multipart/form-data`.
- The proxy rewrites the `Host` header to the Azure endpoint and injects `api-key: <azure-api-key>`.
- The proxy removes internal/legacy query params before forwarding: `target`, `api-version`, `api_version`, `api-key`.
- Successful responses set `X-Azure-Target: <target-name>` so callers can identify the chosen backend.
- Streaming responses are relayed chunk-by-chunk (`io.Copy`), preserving status codes and headers except for hop-by-hop headers.

## Admin Authentication (Session-based)
The admin management system uses **independent username/password authentication**, completely separate from the client proxy bearer-token auth.

- Admin users are stored in `config/admin_users.json` (configured via `config.data_files.admin_users_file`).
- Passwords are stored as `sha256$<salt>$<hex>` hashes.
- After login, the server sets a signed session cookie (`azure_proxy_admin_session` by default).
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
- Web management console entry point (HTML) — left-sidebar navigation with 5 pages: Overview, Clients, Model Costs, Usage Statistics, Audit Logs.
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
- Returns current status for each configured Azure target, including mute windows.
- Example payload:
  ```json
  {
    "status": "ok",
    "checked_at": "2024-05-10T12:00:00Z",
    "targets": [
      {
        "name": "east2",
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
- Returns the current client list from `config.data_files.clients_file`.

### `POST /admin/data/clients`
- Creates a client in the file-backed store.

### `PUT /admin/data/clients/{name}`
- Updates the named client.

### `DELETE /admin/data/clients/{name}`
- Deletes the named client.

### `GET /admin/data/model-costs`
- Returns model token cost configuration from `config.data_files.model_costs_file`.

### `PUT /admin/data/model-costs/{model}` / `DELETE /admin/data/model-costs/{model}`
- Updates or deletes a model cost record.

### `GET /admin/data/usage/events`
- Returns usage events from `config.data_files.usage_events_file` with optional filters (`from`, `to`, `client_name`, `model`, `limit`).

### `GET /admin/data/usage/aggregate`
- Returns aggregated token/cost data by hour or day.

### `GET /admin/data/usage/summary`
- Returns fixed windows for `last_hour`, `yesterday`, and `last_30_days`.

## Error Codes

| Status | Scenario | Notes |
| ------ | -------- | ----- |
| `400 Bad Request` | Malformed client request or unknown `target` value. | Returned before contacting Azure. |
| `401 Unauthorized` | Missing/invalid bearer token. | Includes `WWW-Authenticate: Bearer`. |
| `403 Forbidden` | Authenticated client requested a disallowed target. | Token `allowed_targets` restriction triggered. |
| `404 Not Found` | Admin router fallback or Azure returned 404. | Proxy passes upstream 404 as-is. |
| `429 Too Many Requests` | Propagated from Azure. | Proxy does not alter upstream rate-limit semantics. |
| `500 Internal Server Error` | Unhandled panic recovered by middleware. | Logged with `request_id`. |
| `502 Bad Gateway` | All targets unavailable or upstream returned 5xx. | `X-Azure-Target` still indicates the final attempt. |
| `503 Service Unavailable` | Requested target muted/unavailable during selection. | Client can retry later or omit explicit target. |
| `504 Gateway Timeout` | Transport timeout while contacting Azure. | Triggered by request timeout or network errors. |

The proxy increments internal metrics for total requests, retries, successes, and failures; these counters are exposed via `/admin/metrics`.

## Headers of Interest
- `Authorization: Bearer <token>` (required for authenticated routes)
- `X-Proxy-Target` / `target` query (optional target hint)
- `X-Azure-Target` (response header showing chosen backend)
- `X-Request-ID` (included in every response and log entry)
- `WWW-Authenticate: Bearer` (sent on `401 Unauthorized`)
