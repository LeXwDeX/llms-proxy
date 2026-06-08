# 项目总览

本项目是一个 **多类型上游端点代理服务**：对外提供统一的 HTTP 入口，对内统一转发到多个上游终端（Azure OpenAI、OpenAI、Claude、Gemini、网宿 OpenAI/Claude/Gemini、GitHub Copilot），帮助内部客户端在一个入口下完成鉴权、路由、日志、故障切换与运维管理。

---

## 🚨 Agent 强约束：发布到 33.110

**触发条件**：用户说出任一关键词 —— "发布到 33.110"、"部署到 33.110"、"push 到 33.110"、"上线"、"deploy 到 192.168.33.110"、或类似表达。

**⚠️ 默认部署目标为 8001 测试实例（自 2026-06-08 起）**：Agent 所有发布一律先到 8001 测试端口验证，仅当用户明确说"上线到 8000"/"提升到生产"/"发布到生产"时才部署到 8000 生产实例。

**强制执行顺序（禁止跳步、禁止自创流程）**：

1. **必读**：先读 `docs/部署要求.md` 顶部的「红线清单」和「测试版本部署规则」章节。
2. **本地准备**：
   - `go build ./... && go test ./...` 必须通过才能继续。
   - `git status` 必须干净或用户已确认要提交。
   - 若有改动，先 `git commit` 并 `git push origin main`。
3. **部署唯一命令**（不要自创任何其他命令组合）：
   - **测试实例（默认）**：
     ```bash
     ssh root@192.168.33.110 "cd /DATA/AppData/llms_proxy_test && bash scripts/deploy.sh"
     ```
   - **生产实例（仅用户明确要求时）**：
     ```bash
     ssh root@192.168.33.110 "cd /DATA/AppData/llms_proxy && bash scripts/deploy.sh"
     ```
4. **脚本会自动**：发布锁 → 预检 → 备份到 `/DATA/Backups/llms-proxy/` → `git reset --hard` 目标分支 → 恢复 config/data → 重建容器 → 30 秒健康检查 → 失败时打印回滚命令。
5. **部署后验证**（必做）：
   - 测试：`curl http://192.168.33.110:8001/healthz` → 应返回 `{"status":"ok"}`
   - 生产：`curl http://192.168.33.110:8000/healthz` → 应返回 `{"status":"ok"}`
   - 查看容器日志最后 20 行，确认无 panic / 启动错误。
   - 若用户提到某个具体功能（如 Copilot 某模型），发一次真实请求确认。
6. **重要**：若脚本退出非 0 或健康检查失败，**立即停止**，按脚本输出的回滚命令恢复，不要自作主张继续。
7. **最重要**：当前的模型对话代理服务在 33.110，任何失误操作都会导致当前对话的AGENTS出现故障，切记未经同意就暂停或更改服务。

**红线（绝对禁止）**：

| # | 禁止动作 | 理由 |
|---|---|---|
| ❌ | 在 33.110 上执行 `git clean -fdx` / `git clean -fd` | 会删除 `data/llms-proxy.db`、`config/config.json`、`logs/*`（2026-04-17 事故元凶） |
| ❌ | 绕过 `scripts/deploy.sh` 直接跑 `git reset --hard` + `docker compose up` | 缺少备份和健康检查链路 |
| ❌ | `rm -rf data/`、`rm -rf config/`、`docker compose down -v` | 数据丢失 |
| ❌ | 在 admin UI 里"重置"或"删除"默认 admin 账号 | 会导致下次启动触发 seed 流程 |
| ❌ | 修改 `scripts/deploy.sh` 中备份/恢复/预检环节但不做同步测试 | 任何一处失效都会引发事故 |

**应急（admin 密码忘记）**：

```bash
ssh root@192.168.33.110
cd /DATA/AppData/llms_proxy
docker compose stop
/tmp/reset-admin-pw -db data/llms-proxy.db -list
/tmp/reset-admin-pw -db data/llms-proxy.db -user admin -password <新密码>
docker compose start
```

若 `/tmp/reset-admin-pw` 不存在，从本项目 `scripts/reset-admin-password/` 交叉编译 Linux 版本 scp 上传。

**服务器信息**：

| 项 | 值 |
|---|---|
| 连接 | `ssh root@192.168.33.110` |
| 项目路径 | `/DATA/AppData/llms_proxy/` |
| 对外地址 | `http://192.168.33.110:8000` |
| Admin 后台 | `http://192.168.33.110:8000/admin/`（`admin` / `suntao341`） |
| 备份路径 | `/DATA/Backups/llms-proxy/<时间戳>/`（保留最近 20 份） |
| Docker | 29.1.2（containerd image store） |
| 默认分支 | `origin/main`（脚本自动探测 main/master） |

---

## 业务目标
- 统一入口：屏蔽多个上游终端差异，对外按各协议原生方式透传。
- 多类型上游：通过 `endpoint_type` 字段区分目标类型，支持多种上游，并按类型自动适配认证方式与请求格式。
- 权限隔离：按客户端令牌控制可访问的目标集合。
- 稳定性：支持目标选择、失败静默、重试与自动切换。
- 可观测性：提供结构化日志、健康检查、指标统计与请求 ID 追踪。
- 运维效率：支持配置热加载，减少重启成本。

## 架构分层

### 1. 启动层
- `cmd/proxy/main.go` 是服务入口。
- 负责加载配置、初始化日志、构建认证存储、创建代理服务并挂载路由。
- 默认监听地址和超时由 `config/config.json` 提供，可被环境变量覆盖（如 `SERVER_BIND`、`LOG_LEVEL`）。

### 2. 配置层
- `internal/config` 负责配置读取、校验、克隆和热加载缓存。
- 配置模型包括：
  - `server`：监听地址、请求超时；
  - `targets`：上游终端配置，每个目标包含 `endpoint_type`、端点地址、API Key、允许模型等；`resource_path_prefix` 仅 `azure_openai` 类型必填；
  - `data_store`：嵌入式 bbolt 数据库配置（`db_path` 指定单一 DB 文件路径）；
  - `data_files`（遗留，`omitempty`）：旧版 JSON/JSONL 数据文件路径，仅用于启动时自动迁移到 bbolt；迁移完成后可删除；
  - `admin_session`：管理后台会话配置；
  - `logging`：日志级别与日志文件路径。
- `EndpointType` 常量与辅助函数（`NormalizeEndpointType`、`IsValidEndpointType`）定义在 `config` 包中，作为全局统一的类型标识。

### 3. 认证层
- `internal/auth` 负责客户端鉴权与授权。
- 支持 `Authorization: Bearer <access-key>`、`api-key` header、`x-api-key` header、`x-goog-api-key` header，以及 `?api-key=` 查询参数。
- `Store` 按配置构建运行时凭据映射；`Principal` 保存客户端名称、是否放通全部目标、允许目标列表。

### 4. 代理层
- `internal/proxy/service.go` 是核心转发逻辑。
- 主要职责：
  - 根据客户端权限与请求目标进行路由；
  - 按 `allowed_models` 做模型约束；
  - 按 `endpoint_type` 分支认证逻辑：
    - `azure_openai`（默认）→ 设置 `api-key` header（或 Bearer 直通）；
    - `openai` → 设置 `Authorization: Bearer <key>`；
    - `claude` → 设置 `x-api-key` + 自动补充 `anthropic-version: 2023-06-01`；
    - `gemini` → 设置 `x-goog-api-key` header；
    - `wangsu_openai` / `wangsu_claude` / `wangsu_gemini` → 分别同 `openai` / `claude` / `gemini`；
    - `wangsu_openai_image` / `wangsu_openai_image_edit` → 网宿图像通道（独立终态 URL）；Bearer 认证；buildURL 整体覆盖客户端 path（不做拼接），客户端按 OpenAI 官方 `/v1/images/generations`、`/v1/images/edits` 调用；
    - `deepseek` → 设置 `Authorization: Bearer <key>`；通过 `/deepseek/*` 子路由统一承载 OpenAI 兼容（默认）与 Anthropic 兼容（路径含 `/v1/messages` 时自动加 `/anthropic` 上游前缀）两种格式，详见下方第 9 层；
  - **路径感知路由**（`path_capability.go`）：目标选择时按 `endpoint_type` 检查请求路径兼容性；例如 `wangsu_openai` 仅支持 `/chat/completions`、`/images/generations`、`/embeddings`，不兼容路径的目标自动跳过；
  - **连接粘连**（`affinity.go`）：同客户端 + 同模型的请求倾向路由到同一 target，提升上游 token 缓存（KV cache / prompt cache）命中率；粘连条目 TTL 为 5 分钟，惰性过期；粘连目标不可用或路径不兼容时自动降级为轮询选择；
  - **Copilot 请求拦截**：当模型名带 `Copilot ` 前缀时，请求进入 Copilot 专用处理链路（见下方第 8 层）；
  - 模型名提取支持多种来源：请求体 JSON `model` 字段、Azure 路径 `/deployments/{model}/`、Gemini 路径 `/models/{model}:action`；
  - 用量采集兼容 OpenAI（`usage.prompt_tokens`）、Claude（`usage.input_tokens`）和 Gemini（`usageMetadata.promptTokenCount`/`candidatesTokenCount`）三种响应格式；
  - 对 Azure v1 不兼容字段做白名单过滤（仅对 `azure_openai` 类型目标生效，其他类型透传原始请求体）；
  - 转发请求、回写响应、保留流式输出；
  - 响应头同时返回 `X-Proxy-Target`（规范）和 `X-Azure-Target`（向后兼容）；
  - 记录用量事件时附带 `endpoint_type` 维度；
  - 记录目标健康状态、失败静默与运行时指标。

#### 代理层重试策略（v3，2026-04-24 起）

- 代理层**不做同-target 重试**：上游 4xx/5xx 全部原样透传给客户端，由客户端 SDK（OpenAI 官方 SDK 默认 408/409/429/>=500 重试 2 次 + 指数退避）负责。
- 代理层**保留多-target failover**：仅在网络错误（连接拒绝、DNS、TLS 握手等，非 `context.DeadlineExceeded`）时切换到下一 target——客户端不感知 target 拓扑无法替代。
- 代理层**超时不重试**：由 `TestServiceTimeoutDoesNotRetryOrMute` 红线守护，防止重复扣费/重复推理。
- 默认 `request_timeout_seconds = 1800`（30 分钟），覆盖 gpt-image-2 等长耗时图像生成场景。

### 5. 管理层
- `internal/admin/handler.go` 提供管理接口：
  - `GET /admin/healthz`：健康检查（含各目标状态与 `endpoint_type`）
  - `GET /admin/metrics`：运行时指标
  - `POST /admin/config/reload`：配置热加载
  - Target CRUD：`GET/POST /admin/data/targets`，`PUT/DELETE /admin/data/targets/{name}`
  - Client CRUD：`GET/POST /admin/data/clients`，`PUT/DELETE /admin/data/clients/{name}`
  - Model Costs：`GET /admin/data/model-costs`，`PUT/DELETE /admin/data/model-costs/{model}`
  - Usage：`GET /admin/data/usage/events`，`GET /admin/data/usage/aggregate`，`GET /admin/data/usage/summary`
  - Audit：`GET /admin/data/audit`
  - Catalog API：`GET /admin/data/catalog`（支持 `?endpoint_type=xxx` 过滤），`GET /admin/data/catalog/{endpoint_type}`
- 管理接口用于健康检查、指标查看、配置热加载、数据管理和模型目录查询。
- admin UI 内嵌前端页面，提供「目标管理」界面，支持创建/编辑/删除不同类型的目标。

### 6. 模型目录层
- `internal/catalog/` 提供本地嵌入式模型元数据目录。
- 通过 `go:embed` 嵌入 `data/models.json`，运行时不依赖外部 URL。
- 支持按 `endpoint_type` 查询模型列表、按 `endpoint_type + model` 精确查找、别名解析（`ResolveAlias`）。
- 模型条目包含：`endpoint_type`、`model`、`display_name`、`aliases`、`capabilities`、`default_cost`。
- 费用数据为估算参考值，可能与实际计费存在偏差。

### 7. 基础设施层
- `internal/middleware` 提供请求 ID、panic 恢复和访问日志。
- `internal/logging` 负责错误日志、访问日志与轮转。

### 8. Copilot 集成层
- `internal/copilot/` 提供 GitHub Copilot 上游的专用集成逻辑。
- **OAuth 与 Token 管理**（`oauth.go`、`token.go`）：
  - 通过 GitHub OAuth Device Flow 获取用户授权，交换 Copilot access token；
  - Token 自动刷新（到期前续签），池化管理多个 OAuth 账户。
- **模型管理**（`models.go`）：
  - 下游模型名前缀为 `Copilot `（大写 C + 空格），例如 `Copilot claude-sonnet-4.6`、`Copilot gpt-4o`；
  - `MapModelName` / `ReverseMapModelName` 负责前缀剥离与添加；
  - `ModelMultipliers` 维护模型 premium request 乘数表（免费/低消耗/标准/高消耗分类）；
  - `FetchModelsFromAPI` / `FetchModelDetails` 从 Copilot API 获取实时可用模型列表与元数据。
- **配额管理**（`quota.go`）：跟踪用户 premium request 配额使用情况。
- **请求处理**（`internal/proxy/copilot_handler.go`）：
  - Pool 查找 → 顺序选号 → 动态 Token 注入 → 模型名映射 → 转发 → 额度扣减；
  - 路径规则：`/v1/*` 去除 `/v1` 前缀后透传，其他路径直通；
  - Copilot 上游统一使用 OpenAI 兼容的 `/chat/completions` 端点，无论底层模型是 GPT、Claude 还是 Gemini，客户端需以 OpenAI 格式调用。
- **Premium Request 计费正确性**（`copilot_initiator.go::inferInitiator`）：
  - Copilot 上游用 `X-Initiator: user|agent` HTTP 头决定是否扣 premium request：`user` 扣 1× multiplier，`agent` 不扣，缺失默认按 `user` 扣全额；
  - 代理在所有 Copilot 出站路径（`HandleCopilotPassthrough` / `handleCopilotRequest` / `HandleCopilotModels`）的 `httpClient.Do` 之前注入该头；
  - 客户端已传 `X-Initiator: user|agent`（大小写不敏感）→ 完全尊重原值不覆盖；
  - 客户端未传或非法 → 解析请求体 `messages` 数组最后一条 message：role=tool / role=assistant / Anthropic content blocks 含 tool_result → `agent`；纯 user 文本 → `user`；
  - body 为空或解析失败 → 兜底 `agent`（保守省钱；Copilot ToS 不会因少扣封号，伪造 user 才会封）；
  - 修复了 Claude Code 等 agentic 客户端因不发送此头被全额扣费的问题。

### 9. DeepSeek 集成层
- DeepSeek 官方同时提供 OpenAI 兼容（`https://api.deepseek.com`）与 Anthropic 兼容（`https://api.deepseek.com/anthropic`）两套 API，**同一把 API Key 对两套都有效**。
- 代理在 `cmd/proxy/main.go` 挂 `/deepseek/*` 子路由，运行时强制将 target 选择约束为 `endpoint_type=deepseek`（通过 `internal/proxy/endpoint_hint.go` 的 context hint 机制注入），随后路径自动识别：
  - 路径含 `/v1/messages*` → 上游 path 加 `/anthropic` 前缀（Anthropic 格式）；
  - 其他路径 → 直通（OpenAI 格式）；
  - 路径识别在 `internal/proxy/url.go::buildURL` 中完成，规则函数为 `isAnthropicStylePath`。
- 鉴权统一为 `Authorization: Bearer <key>`（`internal/proxy/forward.go`），与上游真正的 Anthropic 用 `x-api-key` 不同。
- 客户端 SDK 配置：OpenAI SDK / Anthropic SDK 都把 `base_url` 指到 `https://<域名>/deepseek` 即可，`api_key` 填代理客户端 token。
- 多 target 支持：可在 `endpoint_type=deepseek` 下注册多个 target（多 key 池/容灾），标准 affinity + failover 行为生效。
- 回归测试：`internal/proxy/deepseek_test.go`。

## 请求处理链路
1. 请求进入 HTTP Server。
2. 中间件注入 `X-Request-ID`、记录访问日志、兜底 panic。
3. `auth` 中间件校验客户端令牌并写入上下文。
4. 业务路由：
   - `/healthz`：无需鉴权；
   - `/api/ping`：已鉴权后返回客户端信息；
   - `/admin/*`：管理接口；
   - `/deepseek/*`：剥前缀 + 注入 `endpoint_type=deepseek` hint 后进入代理转发；
   - 其余路径进入代理转发。
5. 代理层判断模型类型：
   - 模型名带 `Copilot ` 前缀 → 进入 Copilot 专用链路（Token 池选号、模型名映射、透传转发）；
   - 其他模型 → 按 `endpoint_type` 适配认证与请求体，执行目标选择、重试或 failover。
6. 结果透传给客户端，并在响应头中标记 `X-Proxy-Target`（规范）和 `X-Azure-Target`（向后兼容）。

## 关键业务规则
- 每个目标（Target）通过 `endpoint_type` 标识上游类型。
- 客户端令牌与可访问目标绑定；`allowed_targets` 为空表示允许访问全部目标。
- 显式目标可通过 `X-Proxy-Target` 或 `target` 查询参数指定。
- 若目标配置了 `allowed_models`，请求必须携带可识别的 `model`，且模型必须命中白名单。
- 代理会剥离内部/旧版参数：`target`、`api-version`、`api_version`、`api-key`。
- 对部分 JSON 接口执行顶层字段白名单过滤（chat completions、responses、embeddings），**仅对 `azure_openai` 类型目标生效**；`openai`、`claude`、`gemini`、`deepseek` 等其他类型透传原始请求体。
- **路径兼容性**：按 `endpoint_type` 检查路径兼容性，不兼容的目标自动跳过（详见代理层）。
- **连接粘连**：同客户端 + 同模型倾向路由到同一 target；粘连目标不可用时降级为轮询（详见代理层）。
- 某个目标连续失败后会进入静默窗口，优先切换到其他可用目标。
- 模型费用（`model_costs`）和用量事件（`usage_events`）均包含 `endpoint_type` 维度；`CostTable` 支持双键查找（`endpoint_type:model` → `model` 降级兼容）。
- Copilot 模型通过 `Copilot ` 前缀识别，进入独立的 Token 池化与额度管理链路。

## 目录说明
- `cmd/proxy/`：应用入口。
- `config/`：示例配置。
- `internal/`：核心实现。
  - `internal/config/`：配置读取、校验、热加载；定义 `EndpointType` 常量。
  - `internal/auth/`：客户端鉴权与授权。
  - `internal/proxy/`：核心转发逻辑（含多类型上游适配、路径感知路由 `path_capability.go`、连接粘连 `affinity.go`、Copilot 处理 `copilot_handler.go`）。
  - `internal/copilot/`：GitHub Copilot 专用集成（OAuth、Token 管理、模型管理、配额管理）。
  - `internal/admin/`：管理接口与 admin UI。
  - `internal/catalog/`：嵌入式模型元数据目录（`go:embed data/models.json`），支持按 `endpoint_type` 查询和别名解析。
  - `internal/nosql/`：bbolt 嵌入式 NoSQL 数据存储（clients、model_costs、usage_events、admin_users、admin_audit），单一 DB 文件，启动时支持从旧 JSON 文件自动迁移。
  - `internal/usage/`：用量事件记录与聚合统计。
  - `internal/middleware/`：请求 ID、panic 恢复、访问日志。
  - `internal/logging/`：错误日志、访问日志与轮转。
- `docs/`：接口契约、运维手册、参数白名单、发布说明。
- `deploy/`：部署模板（如 systemd、Docker）。
- `scripts/`：测试与辅助脚本（含 `update-model-catalog.py` 用于更新模型目录数据）。
- `test/`：集成测试。
- `logs/`：默认日志目录。

## 维护建议
- 修改接口行为时，优先同步 `docs/api-contract.md`。
- 修改路由/鉴权/转发行为时，优先检查 `internal/auth`、`internal/proxy`、`internal/admin`。
- 新增或调整 `endpoint_type` 时，需同时更新 `internal/config`（常量与校验）、`internal/proxy`（认证分支）、`internal/proxy/path_capability.go`（路径能力表）、`internal/catalog`（模型数据）。
- 修改 Copilot 集成时，需同时关注 `internal/copilot/`（模型、Token、配额）和 `internal/proxy/copilot_handler.go`（请求处理）。
- 修改模型目录数据时，通过 `scripts/update-model-catalog.py` 生成新的 `internal/catalog/data/models.json`。
- 修改费用或用量逻辑时，注意 `CostTable` 的双键查找机制（`endpoint_type:model` 优先，`model` 降级）。
- 修改日志或运维行为时，检查 `internal/logging` 与 `docs/operations.md`。
- 避免在文档中写入真实密钥、令牌或环境专有敏感信息。

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **LLMs_Proxy** (4484 symbols, 13736 relationships, 300 execution flows). Use the GitNexus MCP tools to understand code, assess impact, and navigate safely.

> If any GitNexus tool warns the index is stale, run `npx gitnexus analyze` in terminal first.

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `gitnexus_impact({target: "symbolName", direction: "upstream"})` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST run `gitnexus_detect_changes()` before committing** to verify your changes only affect expected symbols and execution flows.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `gitnexus_query({query: "concept"})` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `gitnexus_context({name: "symbolName"})`.

## Never Do

- NEVER edit a function, class, or method without first running `gitnexus_impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — use `gitnexus_rename` which understands the call graph.
- NEVER commit changes without running `gitnexus_detect_changes()` to check affected scope.

## Resources

| Resource | Use for |
|----------|---------|
| `gitnexus://repo/LLMs_Proxy/context` | Codebase overview, check index freshness |
| `gitnexus://repo/LLMs_Proxy/clusters` | All functional areas |
| `gitnexus://repo/LLMs_Proxy/processes` | All execution flows |
| `gitnexus://repo/LLMs_Proxy/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
