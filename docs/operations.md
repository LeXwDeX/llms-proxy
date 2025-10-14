# Operations Guide

This guide covers the preparation, deployment, and day-two operations for the Azure OpenAI proxy.

## Pre-Deployment Checklist
- [ ] Collect production Azure OpenAI endpoints, API versions, and API keys.
- [ ] Generate client tokens for each team; scope `allowed_targets` appropriately.
- [ ] Review and customise the checked-in `config/config.json`, then store the runtime copy with restricted file permissions.
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
   Copy `bin/azure-proxy` to the target host if building elsewhere.

2. **Install configuration and logs**
   ```
   /etc/azure-proxy/config.json
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
   - `curl -H "Authorization: Bearer <token>" http://<host>:8080/admin/metrics`
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
1. Edit `config/config.json` and validate JSON.
2. Trigger reload:
   ```sh
   curl -X POST -H "Authorization: Bearer <ops-token>" \
        http://<host>:8080/admin/config/reload
   ```
3. Confirm the new targets and clients via `/admin/healthz`.
4. Roll back by restoring the previous JSON and re-running the reload command if needed.

## Incident Response
1. **Client sees 403** – verify token mapping in `config/config.json` or check `allowed_targets`.
2. **Increased 5xx responses** – inspect `/admin/healthz` for muted targets; investigate upstream Azure incidents.
3. **Proxy unreachable** – check systemd status and logs; restart with `sudo systemctl restart azure-proxy`.
4. **Log growth** – adjust logging config to use rotated paths or compress old logs (see `internal/logging` for options).

## Training Notes
- Share `docs/internal-training.md` with new operators.
- Emphasise use of integration tests in staging before applying production changes.
- Maintain a secure vault for client tokens and Azure API keys; never commit them to version control.
