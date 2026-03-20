# Operations Guide

This guide covers the preparation, deployment, and day-two operations for the Azure OpenAI proxy.

## Pre-Deployment Checklist
- [ ] Collect production Azure OpenAI endpoints, model allowlists (`allowed_models`), and API keys.
- [ ] Generate client tokens for each team; scope `allowed_targets` appropriately and save them to `config/clients.json`.
- [ ] Review and customise the checked-in `config/config.json`, then store the runtime copy with restricted file permissions.
- [ ] Review `config/model_costs.json` and populate model token fees before enabling the cost estimation page.
- [ ] **Configure admin credentials**: edit `config/admin_users.json` to replace the default `admin/admin123` account. Passwords must be hashed as `sha256$<salt>$<hex>`. **Never deploy the default password to production.**
- [ ] **Configure admin session**: set a strong `secret` in `config.admin_session` (at least 32 characters), and enable `secure_cookie: true` when running behind HTTPS.
- [ ] Provision directories for logs (default `logs/`) and ensure the service account has read/write access.
- [ ] Ensure the service account can read/write the `config/` NoSQL files (`clients.json`, `model_costs.json`, `usage_events.jsonl`, `admin_users.json`, `admin_audit.jsonl`).
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
   Copy `bin/azure-proxy` to the target host if building elsewhere.

2. **Install configuration, NoSQL data, and logs**
    ```
    /etc/azure-proxy/config.json
    /etc/azure-proxy/config/clients.json
    /etc/azure-proxy/config/model_costs.json
    /etc/azure-proxy/config/usage_events.jsonl
    /etc/azure-proxy/config/admin_users.json
    /etc/azure-proxy/config/admin_audit.jsonl
    /var/log/azure-proxy/access.log
    /var/log/azure-proxy/error.log
    ```

3. **Systemd service**
   - Copy `deploy/systemd/azure-proxy.service` to `/etc/systemd/system/azure-proxy.service`.
   - Update the `User`, `Group`, binary path, and config path placeholders.
   - Reload units and start the service:
     ```sh
     sudo systemctl daemon-reload
     sudo systemctl enable --now azure-proxy
     ```

4. **Post-deploy validation**
   - `curl http://<host>:8080/healthz`
   - Open `http://<host>:8080/login` in a browser and verify admin login works.
   - Check `/var/log/azure-proxy/error.log` for startup errors.

## Monitoring & Alerting
| Metric / Signal                  | Source               | Recommended Thresholds            |
|----------------------------------|----------------------|-----------------------------------|
| Request success vs failure rate  | `/admin/metrics`     | Alert if failure ratio > 5%       |
| Target mute counts               | `/admin/healthz`     | Alert if all targets muted        |
| Process availability             | `systemd` / supervisor | Alert on service restarts        |
| Disk usage for logs              | OS metrics           | Alert at 80% utilisation          |

Automate polling of admin endpoints or export metrics to Prometheus via a lightweight scraper.

## Configuration Reload Procedure
1. Edit `config/config.json` and/or the file-backed data sources under `config/` (`clients.json`, `model_costs.json`), then validate JSON.
2. Trigger reload:
   ```sh
   curl -X POST -H "Authorization: Bearer <ops-token>" \
        http://<host>:8080/admin/config/reload
   ```
3. Confirm the new targets and clients via `/admin/healthz` and `/admin/data/clients`.
4. Roll back by restoring the previous JSON and re-running the reload command if needed.

## Consumption Tracking & Cost Estimation
- Every successful proxy response will best-effort append a usage event to `config/usage_events.jsonl`.
- Usage events power the `/admin/ui` statistics tab and the `/admin/data/usage/*` APIs.
- If a response does not carry usage data (for example, some streaming or non-standard upstream responses), the proxy skips recording rather than failing the request.
- Model price adjustments should be made in `config/model_costs.json`; the UI uses the file to estimate token cost for the selected time window.

## Incident Response
1. **Client sees 403** – verify token mapping in `config/config.json` or check `allowed_targets`.
2. **Increased 5xx responses** – inspect `/admin/healthz` for muted targets; investigate upstream Azure incidents.
3. **Proxy unreachable** – check systemd status and logs; restart with `sudo systemctl restart azure-proxy`.
4. **Log growth** – adjust logging config to use rotated paths or compress old logs (see `internal/logging` for options).
5. **Client sees 400 model not supported** – verify request `model` is included in at least one target's `allowed_models`.
6. **Statistics page shows zero/empty cost** – confirm `config/model_costs.json` contains the requested model name and non-zero per-token prices.

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

## Training Notes
- Share `docs/internal-training.md` with new operators.
- Emphasise use of integration tests in staging before applying production changes.
- Maintain a secure vault for client tokens and Azure API keys; never commit them to version control.

## Admin Account Management
- Admin accounts are stored in `config/admin_users.json`, separate from client proxy tokens.
- Password format: `sha256$<salt>$<hex>` — generate via `echo -n "<salt><password>" | sha256sum`.
- Default account: `admin` / `admin123`. **Must be changed before production deployment.**
- Session cookie signing uses the `secret` value from `config.admin_session`; rotate this secret periodically.
- Audit events (login, logout, config changes) are recorded to `config/admin_audit.jsonl` and viewable in the admin console's audit page.
- To add a new admin: append a JSON object to `admin_users.json` with `username`, `password_hash`, and `role` fields, then reload config.
