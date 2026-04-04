# Docker Deployment Guide

This guide explains how to package and run the LLMs proxy inside a container. It assumes Docker 24+ (BuildKit enabled) or a compatible container runtime such as nerdctl.

## 1. Build the Image
The repository ships with a multi-stage Dockerfile (`deploy/docker/Dockerfile`) that produces a small Alpine-based image with a statically linked binary.

```sh
docker build \
  -f deploy/docker/Dockerfile \
  -t ycgame/llms-proxy:latest \
  .
```

Arguments:
- `GO_VERSION` (default `1.24.2`) – override the Go toolchain version when needed, for example `--build-arg GO_VERSION=1.25`.

## 2. Prepare Volumes
The container expects three mount points:

| Mount Point | Purpose | Access Mode |
|---|---|---|
| `/etc/llms-proxy` | Configuration directory (`config.json`) | **Read-only** (`:ro`) |
| `/var/lib/llms-proxy` | Data directory (bbolt database file) | **Read-write** |
| `/var/log/llms-proxy` | Log directory (access/error logs) | **Read-write** |

Create directories on the host and copy your config:
```sh
mkdir -p /opt/llms-proxy/config /opt/llms-proxy/data /opt/llms-proxy/logs
cp config/config.json /opt/llms-proxy/config/config.json
# Edit config.json:
#   - Set data_store.db_path to "/var/lib/llms-proxy/llms-proxy.db"
#   - Set logging paths to "/var/log/llms-proxy/access.log" and "/var/log/llms-proxy/error.log"
#   - Add real endpoints, keys, and tokens
```

> **Key change**: the config directory is mounted read-only. All runtime data (clients, model costs, usage events, admin users, audit logs) is stored in the bbolt database file under `/var/lib/llms-proxy`. This separation ensures config immutability while allowing the service to write data.

## 3. Run with `docker run`
```sh
docker run -d \
  --name llms-proxy \
  -p 8000:8000 \
  -v /opt/llms-proxy/config:/etc/llms-proxy:ro \
  -v /opt/llms-proxy/data:/var/lib/llms-proxy \
  -v /opt/llms-proxy/logs:/var/log/llms-proxy \
  ycgame/llms-proxy:latest
```

Environment variables:
- Set `HTTP_PROXY` / `HTTPS_PROXY` if the container must traverse an outbound proxy.
- Set `LOG_LEVEL` to override `logging.level` in `config.json` (supported values: `debug`, `info`, `warn`, `error`).
- Set `SERVER_BIND` to override the listen address (default from `config.json`).

## 4. docker compose Workflow
The repository includes a ready-to-use compose file at `docker-compose.yml`.

```yaml
# docker-compose.yml (excerpt)
services:
  llms-proxy:
    volumes:
      - ./config:/etc/llms-proxy:ro      # config read-only
      - ./data:/var/lib/llms-proxy        # bbolt database (read-write)
      - ./logs:/var/log/llms-proxy        # logs (read-write)
```

1. Prepare your configuration:
   ```sh
   mkdir -p config data logs
   cp config/config.json config/config.json
   # Edit config/config.json as described above
   ```
2. Build the image with compose:
   ```sh
   docker compose build
   ```
3. Bring the stack up:
   ```sh
   docker compose up -d
   ```
4. Verify the container:
   ```sh
   docker compose ps
   docker compose logs -f
   ```
5. Tear down when required:
   ```sh
   docker compose down
   ```

Because the container runs as a non-root user (`llmsproxy`), the host directories for data and logs must be writable by the container user. If startup logs show `permission denied`, verify host permissions:

```sh
ls -ld ./config ./data ./logs
docker compose exec llms-proxy sh -lc 'id && ls -ld /etc/llms-proxy /var/lib/llms-proxy /var/log/llms-proxy'
```

## 5. Migration from JSON Files
If upgrading from a file-based deployment, the service automatically migrates data from old JSON/JSONL files to bbolt on first startup:

1. Keep the `data_files` section in `config.json` with paths pointing to the old files.
2. Mount the directory containing old files so they are accessible inside the container.
3. On first startup, the service detects the empty bbolt database and imports all data from the JSON files.
4. The migration is **idempotent** — running it multiple times is safe. Old files are preserved.
5. After confirming the migration succeeded, the `data_files` section can be removed from config.

## 6. Operational Notes
- The container runs as a non-root user (`llmsproxy`) and exposes port 8000 by default.
- Configure log rotation on the host if `/var/log/llms-proxy` grows quickly, or change the log paths in the mounted `config.json`.
- Use the admin API (`/admin/healthz`, `/admin/metrics`, `/admin/config/reload`) the same way as the bare-metal deployment.
- **Data backup**: regularly back up the bbolt database file from the data volume (`/var/lib/llms-proxy/llms-proxy.db`). Stop the container or use filesystem snapshots for a consistent copy.
- To upgrade: rebuild the image and redeploy; all state is in the data volume, not inside the container.
- For Kubernetes, adapt the same volumes using ConfigMap/Secret for config (read-only) and a persistent volume claim for the data directory.
