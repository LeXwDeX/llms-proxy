# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.24.2

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /workspace

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Auto-update model catalog (best-effort: failure keeps existing data)
RUN pip install --no-cache-dir --break-system-packages requests 2>/dev/null; \
    if curl -sSf --connect-timeout 10 --max-time 30 https://models.dev/api.json -o /tmp/models_dev_raw.json 2>/dev/null; then \
      python3 scripts/update-model-catalog.py /tmp/models_dev_raw.json internal/catalog/data/models.json \
      && echo ">> catalog updated" \
      || echo ">> catalog transform failed, keeping existing"; \
      rm -f /tmp/models_dev_raw.json; \
    else \
      echo ">> catalog download failed, keeping existing data"; \
    fi

RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o /workspace/bin/llms-proxy ./cmd/proxy

FROM alpine:3.19
LABEL org.opencontainers.image.title="LLMs Proxy" \
      org.opencontainers.image.description="Lightweight proxy with auth, logging, and failover for LLMs Endpoints" \
      org.opencontainers.image.source="https://github.com/ycgame/llms-proxy"

RUN apk add --no-cache su-exec && \
    addgroup -S llmsproxy && adduser -S llmsproxy -G llmsproxy && \
    mkdir -p /etc/llms-proxy /var/log/llms-proxy /var/lib/llms-proxy && \
    chown -R llmsproxy:llmsproxy /etc/llms-proxy /var/log/llms-proxy /var/lib/llms-proxy

WORKDIR /app
COPY --from=build /workspace/bin/llms-proxy /usr/local/bin/llms-proxy
COPY deploy/docker/entrypoint.sh /usr/local/bin/entrypoint.sh

# 不再硬编码 USER，由 entrypoint.sh 在运行时降权。
# 容器默认以 root 启动 → entrypoint 修复挂载卷权限 → su-exec 降权到 llmsproxy。

EXPOSE 8080
VOLUME ["/etc/llms-proxy", "/var/log/llms-proxy", "/var/lib/llms-proxy"]

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["/usr/local/bin/llms-proxy", "-config", "/etc/llms-proxy/config.json"]
