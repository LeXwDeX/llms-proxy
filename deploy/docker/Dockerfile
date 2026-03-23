# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.24.2

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /workspace

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o /workspace/bin/llms-proxy ./cmd/proxy

FROM alpine:3.19
LABEL org.opencontainers.image.title="LLMs Proxy" \
      org.opencontainers.image.description="Lightweight proxy with auth, logging, and failover for LLMs Endpoints" \
      org.opencontainers.image.source="https://github.com/ycgame/llms-proxy"

RUN addgroup -S llmsproxy && adduser -S llmsproxy -G llmsproxy && \
    mkdir -p /etc/llms-proxy /var/log/llms-proxy && \
    chown -R llmsproxy:llmsproxy /etc/llms-proxy /var/log/llms-proxy

WORKDIR /app
COPY --from=build /workspace/bin/llms-proxy /usr/local/bin/llms-proxy

USER llmsproxy:llmsproxy

EXPOSE 8080
VOLUME ["/etc/llms-proxy", "/var/log/llms-proxy"]

ENTRYPOINT ["/usr/local/bin/llms-proxy"]
CMD ["-config", "/etc/llms-proxy/config.json"]
