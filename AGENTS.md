# 项目总览

本项目是一个 **多类型上游端点代理服务**：对外提供统一的 HTTP 入口，对内统一转发到多个上游终端（Azure OpenAI、OpenAI、Claude、Gemini），帮助内部客户端在一个入口下完成鉴权、路由、日志、故障切换与运维管理。

## 业务目标
- 统一入口：屏蔽多个上游终端差异（Azure OpenAI / OpenAI / Claude / Gemini），对外按各协议原生方式透传。
- 多类型上游：通过 `endpoint_type` 字段区分目标类型，支持 `azure_openai`（默认）、`openai`、`claude` 、`gemini` 等多种上游，并按类型自动适配认证方式与请求格式。
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
  - `targets`：上游终端配置（向后兼容旧键名 `azure_targets`），每个目标包含 `endpoint_type`（`azure_openai` | `openai` | `claude` | `gemini`，默认 `azure_openai`）、端点地址、API Key、允许模型等；`resource_path_prefix` 仅 `azure_openai` 类型必填；
  - `data_files`：文件式 NoSQL 数据路径（clients、model_costs、usage_events、admin_users、admin_audit）；
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
  - 模型名提取支持多种来源：请求体 JSON `model` 字段、Azure 路径 `/deployments/{model}/`、Gemini 路径 `/models/{model}:action`；
  - 用量采集兼容 OpenAI（`usage.prompt_tokens`）、Claude（`usage.input_tokens`）和 Gemini（`usageMetadata.promptTokenCount`/`candidatesTokenCount`）三种响应格式；
  - 对 Azure v1 不兼容字段做白名单过滤（仅对 `azure_openai` 类型目标生效，其他类型透传原始请求体）；
  - 转发请求、回写响应、保留流式输出；
  - 响应头同时返回 `X-Proxy-Target`（规范）和 `X-Azure-Target`（向后兼容）；
  - 记录用量事件时附带 `endpoint_type` 维度；
  - 记录目标健康状态、失败静默与运行时指标。

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

## 请求处理链路
1. 请求进入 HTTP Server。
2. 中间件注入 `X-Request-ID`、记录访问日志、兜底 panic。
3. `auth` 中间件校验客户端令牌并写入上下文。
4. 业务路由：
   - `/healthz`：无需鉴权；
   - `/api/ping`：已鉴权后返回客户端信息；
   - `/admin/*`：管理接口；
   - 其余路径进入代理转发。
5. 代理层根据目标/模型/权限选择上游后端，按 `endpoint_type` 适配认证与请求体，必要时执行重试或 failover。
6. 结果透传给客户端，并在响应头中标记 `X-Proxy-Target`（规范）和 `X-Azure-Target`（向后兼容）。

## 关键业务规则
- 每个目标（Target）通过 `endpoint_type` 标识上游类型，支持 `azure_openai`（默认）、`openai`、`claude`、`gemini`；空值等同于 `azure_openai`。
- 客户端令牌与可访问目标绑定；`allowed_targets` 为空表示允许访问全部目标。
- 显式目标可通过 `X-Proxy-Target` 或 `target` 查询参数指定。
- 若目标配置了 `allowed_models`，请求必须携带可识别的 `model`，且模型必须命中白名单。
- 代理会剥离内部/旧版参数：`target`、`api-version`、`api_version`、`api-key`。
- 对部分 JSON 接口执行顶层字段白名单过滤（chat completions、responses、embeddings），**仅对 `azure_openai` 类型目标生效**；`openai`、`claude` 和 `gemini` 类型透传原始请求体。
- 某个目标连续失败后会进入静默窗口，优先切换到其他可用目标。
- 模型费用（`model_costs`）和用量事件（`usage_events`）均包含 `endpoint_type` 维度；`CostTable` 支持双键查找（`endpoint_type:model` → `model` 降级兼容）。

## 目录说明
- `cmd/proxy/`：应用入口。
- `config/`：示例配置。
- `internal/`：核心实现。
  - `internal/config/`：配置读取、校验、热加载；定义 `EndpointType` 常量。
  - `internal/auth/`：客户端鉴权与授权。
  - `internal/proxy/`：核心转发逻辑（含多类型上游适配）。
  - `internal/admin/`：管理接口与 admin UI。
  - `internal/catalog/`：嵌入式模型元数据目录（`go:embed data/models.json`），支持按 `endpoint_type` 查询和别名解析。
  - `internal/nosql/`：文件式 NoSQL 数据存储（clients、model_costs、admin_users 等）。
  - `internal/usage/`：用量事件记录与聚合统计。
  - `internal/middleware/`：请求 ID、panic 恢复、访问日志。
  - `internal/logging/`：错误日志、访问日志与轮转。
- `docs/`：接口契约、运维手册、参数白名单、发布说明。
- `deploy/`：部署模板（如 systemd）。
- `scripts/`：测试与辅助脚本（含 `update-model-catalog.py` 用于更新模型目录数据）。
- `test/`：集成测试。
- `logs/`：默认日志目录。

## 维护建议
- 修改接口行为时，优先同步 `docs/api-contract.md`。
- 修改路由/鉴权/转发行为时，优先检查 `internal/auth`、`internal/proxy`、`internal/admin`。
- 新增或调整 `endpoint_type` 时，需同时更新 `internal/config`（常量与校验）、`internal/proxy`（认证分支）、`internal/catalog`（模型数据）。
- 修改模型目录数据时，通过 `scripts/update-model-catalog.py` 生成新的 `internal/catalog/data/models.json`。
- 修改费用或用量逻辑时，注意 `CostTable` 的双键查找机制（`endpoint_type:model` 优先，`model` 降级）。
- 修改日志或运维行为时，检查 `internal/logging` 与 `docs/operations.md`。
- 避免在文档中写入真实密钥、令牌或环境专有敏感信息。

## 适用范围
本文件用于帮助后续维护者和自动化代理快速理解项目结构与业务边界；如实现发生变化，应同步更新本文件与对应设计文档。
