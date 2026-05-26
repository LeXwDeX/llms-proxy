#
# ⚠️ 构建必须使用 --no-cache，否则 BuildKit 会缓存旧二进制！
#   docker build --no-cache -t llms-proxy:latest -f deploy/docker/Dockerfile .
#

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

RUN mkdir -p /etc/llms-proxy /var/log/llms-proxy /var/lib/llms-proxy

WORKDIR /app
COPY --from=build /workspace/bin/llms-proxy /usr/local/bin/llms-proxy

EXPOSE 8080
VOLUME ["/etc/llms-proxy", "/var/log/llms-proxy", "/var/lib/llms-proxy"]

CMD ["/usr/local/bin/llms-proxy", "-config", "/etc/llms-proxy/config.json"]
