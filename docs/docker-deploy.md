# Docker Deployment Guide

This guide explains how to package and run the Azure OpenAI proxy inside a container. It assumes Docker 24+ (BuildKit enabled) or a compatible container runtime such as nerdctl.

## 1. Build the Image
The repository ships with a multi-stage Dockerfile (`deploy/docker/Dockerfile`) that produces a small Alpine-based image with a statically linked binary.

```sh
docker build \
  -f deploy/docker/Dockerfile \
  -t ycgame/azure-proxy:latest \
  .
```

Arguments:
- `GO_VERSION` (default `1.22`) – set when you need to build with a newer Go toolchain, for example `--build-arg GO_VERSION=1.23`.

## 2. Prepare Configuration & Log Volumes
The container expects:
- `/etc/azure-proxy/config.json` – mounted configuration file containing Azure endpoints, clients, and logging paths.
- `/var/log/azure-proxy` – writable directory for access/error logs (matches defaults in `config/config.json`).

Create directories on the host and copy your config:
```sh
mkdir -p /opt/azure-proxy
cp config/config.json /opt/azure-proxy/config.json
# Edit config.json to include real endpoints, keys, and tokens.
mkdir -p /var/log/azure-proxy
```

## 3. Run with `docker run`
```sh
docker run -d \
  --name azure-proxy \
  -p 8000:8000 \
  -v /opt/azure-proxy/config.json:/etc/azure-proxy/config.json:ro \
  -v /var/log/azure-proxy:/var/log/azure-proxy \
  ycgame/azure-proxy:latest
```

Environment variables:
- Set `HTTP_PROXY` / `HTTPS_PROXY` if the container must traverse an outbound proxy.

Logging level is governed by the `logging.level` field inside `config.json`; the binary does not currently honour a `LOG_LEVEL` environment variable.

## 4. docker compose Workflow
The repository includes a ready-to-use compose file at `docker-compose.yml` and an environment template `.env.example`.

1. Prepare environment variables (optional):
   ```sh
   cp .env.example .env
   # Edit .env to point CONFIG_DIR / LOG_PATH to your host directories and set PROXY_PORT if needed.
   ```
2. Build the image with compose:
   ```sh
   docker compose build
   ```
   This reuses `deploy/docker/Dockerfile`.
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

Ensure the directory referenced by `CONFIG_DIR` contains the `config.json` file so `/etc/azure-proxy/config.json` resolves inside the container. `PROXY_PORT` updates both the published port and the container's `SERVER_BIND`, so startup logs reflect the client-facing port.

## 5. Operational Notes
- The container runs as a non-root user (`azureproxy`) and exposes the `PROXY_PORT` value (default `8000`).
- Configure log rotation on the host if `/var/log/azure-proxy` grows quickly, or change the log paths in the mounted `config.json`.
- Use the admin API (`/admin/healthz`, `/admin/metrics`, `/admin/config/reload`) the same way as the bare-metal deployment. Tokens are identical.
- To upgrade: `docker pull ycgame/azure-proxy:<tag>` and redeploy; no state is stored inside the container.
- For Kubernetes, adapt the same volumes using ConfigMap/Secret for `config.json` and a persistent volume claim for logs.
