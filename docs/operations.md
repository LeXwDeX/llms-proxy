# Operations Guide

This guide covers the preparation, deployment, and day-two operations for the Azure OpenAI proxy.

> **Multi-Endpoint Support** — The proxy supports multiple upstream provider
> types via the `endpoint_type` field on each target: `azure_openai` (default),
> `openai`, `claude`, `gemini`, `wangsu_openai` / `wangsu_claude` / `wangsu_gemini`,
> `wangsu_openai_image` / `wangsu_openai_image_edit`, `copilot`, and `deepseek`.
> The authoritative list is exposed at runtime via `GET /admin/data/endpoint-types`.
> All operational procedures below apply to every type unless stated otherwise.

## Pre-Deployment Checklist
- [ ] Collect production Azure OpenAI endpoints, model allowlists (`allowed_models`), and API keys.
- [ ] If routing to **OpenAI** upstream, prepare an OpenAI API Key (starts with `sk-`).
- [ ] If routing to **Claude** upstream, prepare an Anthropic API Key (starts with `sk-ant-`).
- [ ] If routing to **DeepSeek** upstream, prepare a DeepSeek API Key. The same key works for both the OpenAI-compatible and Anthropic-compatible surfaces; the proxy mounts a single `/deepseek/*` sub-route that auto-detects the format from the request path (paths matching `/v1/messages` are forwarded to the Anthropic-compatible surface, all others to the OpenAI-compatible surface). See `docs/api-contract.md` → "DeepSeek dual-format sub-route" for details.
- [ ] Generate client tokens for each team; scope `allowed_targets` appropriately (clients are managed via the admin UI or API at `/admin/data/clients`).
- [ ] Review and customise the checked-in `config/config.json`, then store the runtime copy with restricted file permissions. Each entry in `targets` carries an `endpoint_type` field — see the multi-endpoint support note above for accepted values.
- [ ] Configure `data_store.db_path` — the path to the bbolt database file (default `llms-proxy.db`, relative to config directory). Ensure the service account has read/write access to this path.
- [ ] (Migration) If upgrading from a file-based deployment, keep old `data_files` paths in config for one-time automatic migration. On first startup with bbolt, existing JSON/JSONL data will be imported. Old files are preserved as backups.
- [ ] Model costs can be managed via the admin UI at `/admin/data/model-costs`. Each cost record includes an `endpoint_type` field (defaults to `azure_openai` for backward compatibility).
- [ ] **Configure admin credentials**: the default admin account (`admin` / `admin123`) is seeded automatically when the user store is empty on first startup. **Change the password immediately in production** via the admin UI.
- [ ] **Configure admin session**: set a strong `secret` in `config.admin_session` (at least 32 characters), and enable `secure_cookie: true` when running behind HTTPS.
- [ ] Provision directories for logs (default `logs/`) and ensure the service account has read/write access.
- [ ] Validate configuration locally:
  ```sh
  make test
  ./scripts/run-integration-tests.sh
  ```
- [ ] (Optional) If deploying in containers, review `docs/docker-deploy.md` and prepare the required volumes.

## Deployment
1. **Build the binary**
   ```sh
   make build
   ```
   Copy `bin/llms-proxy` to the target host if building elsewhere.

2. **Install configuration, database, and logs**
    ```
    /etc/llms-proxy/config.json       # Configuration (read-only at runtime)
    /var/lib/llms-proxy/llms-proxy.db  # bbolt database (read-write)
    /var/log/llms-proxy/access.log
    /var/log/llms-proxy/error.log
    ```
    Set `data_store.db_path` in `config.json` to point to the database file path (e.g., `/var/lib/llms-proxy/llms-proxy.db`). Ensure the directory exists and the service account has write access.

3. **Systemd service**
   - Copy `deploy/systemd/llms-proxy.service` to `/etc/systemd/system/llms-proxy.service`.
   - Update the `User`, `Group`, binary path, and config path placeholders.
   - Reload units and start the service:
     ```sh
     sudo systemctl daemon-reload
     sudo systemctl enable --now llms-proxy
     ```

4. **Post-deploy validation**
   - `curl http://<host>:8080/healthz`
   - Open `http://<host>:8080/login` in a browser and verify admin login works.
   - Check `/var/log/llms-proxy/error.log` for startup errors.

## Monitoring & Alerting
| Metric / Signal                  | Source               | Recommended Thresholds            |
|----------------------------------|----------------------|-----------------------------------|
| Request success vs failure rate  | `/admin/metrics`     | Alert if failure ratio > 5%       |
| Target mute counts               | `/admin/healthz`     | Alert if all targets muted        |
| Process availability             | `systemd` / supervisor | Alert on service restarts        |
| Disk usage for logs              | OS metrics           | Alert at 80% utilisation          |

Automate polling of admin endpoints or export metrics to Prometheus via a lightweight scraper.

## Configuration Reload Procedure
1. Edit `config/config.json` (targets, logging, etc.), then validate JSON. Alternatively, manage targets through the admin UI at `/admin/data/targets` (supports full CRUD — create, read, update, delete). Client and model cost data is stored in the bbolt database and managed via the admin UI or API; no file editing is needed for these.
2. Trigger reload:
   ```sh
   curl -X POST -H "Authorization: Bearer <ops-token>" \
        http://<host>:8080/admin/config/reload
   ```
3. Confirm the new targets and clients via `/admin/healthz`, `/admin/data/clients`, and `/admin/data/targets`.
4. Roll back by restoring the previous JSON and re-running the reload command if needed.

## Consumption Tracking & Cost Estimation
- Every successful proxy response will best-effort record a usage event to the bbolt database.
- Usage events power the `/admin/` statistics tab and the `/admin/data/usage/*` APIs.
- If a response does not carry usage data (for example, some streaming or non-standard upstream responses), the proxy skips recording rather than failing the request.
- Cost tracking is keyed by **`endpoint_type` + `model`**. The same model name under different endpoint types (e.g., `gpt-4o` via `azure_openai` vs `openai`) is tracked and priced independently.
- Model price adjustments can be made via the admin UI (`/admin/data/model-costs`) or the REST API. Each record carries an `endpoint_type` field (default `azure_openai`, backward compatible).
- The UI uses the cost data to estimate token cost for the selected time window.

## Incident Response
1. **Client sees 403** – verify token mapping in `config/config.json` or check `allowed_targets`.
2. **Increased 5xx responses** – inspect `/admin/healthz` for muted targets; investigate upstream Azure incidents.
3. **Proxy unreachable** – check systemd status and logs; restart with `sudo systemctl restart llms-proxy`.
4. **Log growth** – adjust logging config to use rotated paths or compress old logs (see `internal/logging` for options).
5. **Client sees 400 model not supported** – verify request `model` is included in at least one target's `allowed_models`.
6. **Statistics page shows zero/empty cost** – confirm model cost records exist in the database (via `/admin/data/model-costs`) with the correct model name and non-zero per-token prices.
7. **Proxy returns upstream error** – verify the target's `endpoint_type` is set correctly (`azure_openai`, `openai`, `claude`, or `gemini`). Confirm the API key matches the upstream service (Azure keys for `azure_openai`, OpenAI `sk-` keys for `openai`, Anthropic `sk-ant-` keys for `claude`, Google `AIza` keys for `gemini`).
8. **Claude / OpenAI / Gemini authentication failure** – check that the key format matches the target type. OpenAI keys use the `sk-` prefix; Anthropic (Claude) keys use the `sk-ant-` prefix; Google (Gemini) keys use the `AIza` prefix. A mismatch between key and `endpoint_type` will cause 401/403 from the upstream provider.

## Azure v1 Endpoint-Model Verification
在正式切流前，建议逐个校验 endpoint+model 组合是否可用（Azure v1）：

```sh
curl -sS \
  -H "api-key: <azure-api-key>" \
  "https://<resource>.openai.azure.com/openai/v1/models/<model-name>"
```

- `200` 表示该模型在该 endpoint 可用。
- `404/400` 通常表示该模型未部署或该 endpoint 不支持该模型。
- `401/403` 通常表示密钥或权限问题。

## Model Catalog Operations

The project ships an **embedded model catalog** (`internal/catalog/data/models.json`) generated from models.dev. Known providers are mapped to existing endpoint types, and unknown upstream providers are kept with their provider id as `endpoint_type` so newly added providers are not dropped during build-time refreshes. The catalog provides default cost data, display names, capability tags, and model aliases.

### How the catalog is used
- The catalog is compiled into the binary via `go:embed` — no external network calls are needed at runtime.
- When a model cost entry is missing from the database, the admin UI can show the catalog's default cost as a reference.
- Model aliases (e.g., `claude-3.5-sonnet-20241022` → `claude-3-5-sonnet-20241022`) are resolved automatically.
- Browse the catalog in the admin UI at `/admin/data/catalog` (all types) or `/admin/data/catalog/{endpoint_type}` (filtered).

### Updating the catalog
1. Fetch the latest upstream data:
   ```sh
   curl -sS https://models.dev/api.json -o /tmp/models_dev_raw.json
   ```
2. Run the update script:
   ```sh
   python3 scripts/update-model-catalog.py /tmp/models_dev_raw.json internal/catalog/data/models.json
   ```
   The script converts prices from $/million tokens, maps known providers to existing `endpoint_type` values (`openai` → `openai`, `azure` → `azure_openai`, `anthropic` → `claude`, `google` → `gemini`), keeps unknown providers instead of trimming them, and supplements only project-specific compatibility entries that may be missing from the upstream data.
3. Rebuild the binary (the JSON is embedded at compile time):
   ```sh
   make build
   ```
4. Redeploy and verify with:
   ```sh
   curl http://<host>:8080/admin/data/catalog | jq length
   ```

> **Note:** The catalog provides *reference* pricing only. Production billing
> should always rely on model cost entries maintained by the operations team
> via the admin UI or API (`/admin/data/model-costs`).

## Training Notes
- Share `docs/internal-training.md` with new operators.
- Emphasise use of integration tests in staging before applying production changes.
- Maintain a secure vault for client tokens and Azure API keys; never commit them to version control.

## Admin Account Management
- Admin accounts are stored in the bbolt database, separate from client proxy tokens.
- Password format: `sha256$<salt>$<hex>`.
- Default account: `admin` / `admin123` is seeded automatically when the database is empty. **Must be changed before production deployment.**
- Session cookie signing uses the `secret` value from `config.admin_session`; rotate this secret periodically.
- Audit events (login, logout, config changes) are recorded to the bbolt database and viewable in the admin console's audit page.
- To add a new admin: use the admin API or manage accounts through the admin UI.

## Data Backup & Recovery
- **bbolt database**: the single DB file (configured via `data_store.db_path`) contains all runtime data. Back up this file regularly using filesystem snapshots or file copy while the service is stopped.
- **Hot backup**: bbolt supports read transactions while the service is running, but for a consistent backup it is recommended to stop the service briefly or use filesystem-level snapshots.
- **Recovery**: restore the DB file from backup and restart the service. The bbolt database is self-contained; no additional files are needed.
- **Migration from JSON files**: if `data_files` paths are present in config and the bbolt database has not yet been migrated, the service will automatically import data on startup. This is a one-time, idempotent operation. Old JSON/JSONL files are preserved and can serve as an additional backup.
