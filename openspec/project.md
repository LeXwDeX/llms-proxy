# Project Context

## Purpose
本项目是一个面向内部客户端的轻量级 HTTP 代理，用于统一转发多个 Azure OpenAI 终端（endpoints）。代理集中管理 Azure API Key 与内部访问令牌（client access key），提供请求路由、结构化日志、基本故障切换与运维管理接口，使调用方以“单一入口”访问多套 Azure 资源。

## Tech Stack
- Go（模块：`github.com/ycgame/azure-proxy`；`go.mod` 指定 Go `1.24.2`）。
- HTTP 框架：`github.com/go-chi/chi/v5`（路由与中间件组合）。
- 日志：Go `log/slog` + `lumberjack` 轮转（`gopkg.in/natefinch/lumberjack.v2`）。
- 运行方式：本地二进制（`make build`）/ systemd（`deploy/systemd/azure-proxy.service`）/ Docker 多阶段构建（`deploy/docker/Dockerfile`）/ `docker compose`（`docker-compose.yml`）。
- 配置：JSON（示例：`config/config.json`），支持管理接口热加载（`POST /admin/config/reload`）。

## Project Conventions

### Code Style
- 语言与格式：遵循 Go 生态标准（`gofmt`/`go test`），避免非必要的技巧性写法。
- 包结构：入口在 `cmd/proxy/main.go`，业务实现放在 `internal/*`（按领域拆分：`auth`、`config`、`proxy`、`middleware`、`logging`、`admin`）。
- 日志：使用结构化字段（`slog.Logger`），统一通过 `internal/logging` 初始化 access/error/app logger。
- 错误处理：优先返回明确的 HTTP 状态码与简短消息；内部细节写入结构化日志（包含 `request_id`）。

### Architecture Patterns
- 组合式中间件：`RequestID` → `Recoverer` → `AccessLogger`（见 `cmd/proxy/main.go`）。
- 鉴权先于转发：除 `/healthz` 外，所有请求进入 `auth.Middleware`；其余路径由 `proxy.Service` 作为 `NotFound` fallback 进行透传。
- 配置管理：`internal/config.Manager` 负责读取、校验、缓存与热替换；管理接口 reload 时先校验新配置并支持失败回滚。
- 目标选择与故障切换：`internal/proxy.Service` 维护目标状态（失败计数、静默窗口、RR 计数器），在网络错误/可重试错误时自动重试并切换。
- 兼容 Azure 语义：对外保持 Azure OpenAI 的请求/响应形态，代理仅做鉴权、路由、头部重写与观测增强（如 `X-Request-ID`、`X-Azure-Target`）。

### Testing Strategy
- 单元测试：`go test ./...`（`make test`）。
- 集成测试：使用 build tag `integration`（`go test -tags=integration ./test/...` 或 `./scripts/run-integration-tests.sh`）。
- 新增行为优先补测试：核心逻辑（鉴权、配置校验、目标选择/重试）优先用单元测试覆盖；涉及真实转发/静默窗口的场景使用集成测试验证。

### Git Workflow
- 本仓库未内置强约束（如 commit lint）。建议：功能分支 → PR → review → 合并（squash 或 merge 按团队规范）。
- 变更较大/新增能力：先走 OpenSpec proposal（见 `openspec/AGENTS.md`），proposal 通过后再实现。

## Domain Context
- 访问模型：内部客户端使用 `Authorization: Bearer <access-key>` 或 `api-key: <access-key>` 鉴权；`access-key` 对应 `config.clients[*]`（并可通过 `allowed_targets` 限定可访问的 Azure 目标）。
- 目标选择：客户端可用 `X-Proxy-Target: <target-name>` 或 `?target=<target-name>` 指定目标；未指定则允许自动选择与回退。
- 透传：除 hop-by-hop 头部外，代理尽量保持上游响应；流式响应按 chunk 转发。
- 运维接口（需鉴权）：`/admin/healthz`（目标状态/静默窗口）、`/admin/metrics`（聚合计数）、`/admin/config/reload`（热加载）。更多约定见 `docs/api-contract.md`。

## Important Constraints
- 安全：Azure API Key 与 client token 属于敏感信息，生产环境必须使用安全存储（如 Secret/Vault），避免写入代码与版本库；日志中不得泄露凭据。
- 可靠性：当上游网络异常或目标被静默时，应保证可观测性（`request_id`、`X-Azure-Target`、管理接口）并尽量快速回退到可用目标。
- 可操作性：支持不重启热加载配置；部署需满足日志目录可写、配置文件可读的最小权限原则。

## External Dependencies
- Azure OpenAI endpoints（`config.azure_targets[*].endpoint`）、模型白名单（`allowed_models`）与 API Key（`azure_api_key`）。
- 运行环境：systemd 或容器运行时（Docker/nerdctl），以及宿主机日志与配置挂载目录。
- 可选：出站代理（`HTTP_PROXY`/`HTTPS_PROXY`），由 Go `http.Transport` 自动读取环境变量。
