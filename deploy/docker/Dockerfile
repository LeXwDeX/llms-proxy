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
    go build -o /workspace/bin/azure-proxy ./cmd/proxy

FROM alpine:3.19
LABEL org.opencontainers.image.title="LLMs Proxy" \
      org.opencontainers.image.description="Lightweight proxy with auth, logging, and failover for LLMs Endpoints" \
      org.opencontainers.image.source="https://github.com/ycgame/azure-proxy"

RUN addgroup -S azureproxy && adduser -S azureproxy -G azureproxy && \
    mkdir -p /etc/azure-proxy /var/log/azure-proxy && \
    chown -R azureproxy:azureproxy /etc/azure-proxy /var/log/azure-proxy

WORKDIR /app
COPY --from=build /workspace/bin/azure-proxy /usr/local/bin/azure-proxy

USER azureproxy:azureproxy

EXPOSE 8080
VOLUME ["/etc/azure-proxy", "/var/log/azure-proxy"]

ENTRYPOINT ["/usr/local/bin/azure-proxy"]
CMD ["-config", "/etc/azure-proxy/config.json"]
