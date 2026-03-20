# 发现记录

## 项目定位
- 仓库实现的是一个 **Azure OpenAI 代理服务**：对外提供 OpenAI 风格入口，对内统一转发到多个 Azure OpenAI 终端。
- README 明确说明其目标包括：集中管理凭据、请求路由、结构化日志、故障切换和管理接口。

## 架构证据
- `cmd/proxy/main.go`：程序入口，负责加载配置、初始化日志、构建认证存储、初始化代理服务并注册路由。
- `internal/config/config.go`：配置模型包含 `server`、`azure_targets`、`clients`、`logging`，并提供热加载与校验。
- `internal/auth/*`：实现客户端令牌认证、Principal 上下文注入、目标访问控制。
- `internal/proxy/service.go`：核心转发逻辑，负责目标选择、模型校验、请求清洗、故障切换、流式响应透传和运行时指标。
- `internal/admin/handler.go`：管理接口，提供健康检查、指标和配置重载。
- `internal/logging/logging.go`：访问日志与错误日志分离，支持轮转。
- `internal/middleware/middleware.go`：请求 ID、panic 恢复、访问日志。

## 业务规则摘要
- 客户端通过 `Authorization: Bearer <access-key>`、`api-key` header 或 `?api-key=` 进行认证；凭据在配置中按客户端维护。
- 客户端可被限制访问部分 Azure target；若 `allowed_targets` 为空，则默认放通全部目标。
- 代理支持显式 target 指定（`X-Proxy-Target` / `target` query）与自动择优/故障切换。
- 若 target 配置了 `allowed_models`，请求必须携带可识别的 `model`，且模型需命中白名单。
- 对 `/v1/chat/completions`、`/v1/responses`、`/v1/embeddings` 等 JSON 接口做顶层参数白名单过滤，避免向 Azure v1 发送不兼容字段。
- 代理会剥离内部/旧版参数（`target`、`api-version`、`api_version`、`api-key`），并在返回中标记 `X-Azure-Target`。

## 运行与运维
- `/healthz` 为无鉴权健康检查；`/api/ping` 用于已鉴权客户端探测连通性。
- `/admin/healthz`、`/admin/metrics`、`/admin/config/reload` 需要鉴权，提供可观测性与热加载能力。
- 日志默认写入访问日志与错误日志文件，支持轮转。

## 本轮验证状态
- 已完成源码与 README / docs 的交叉阅读。
- 本轮未执行构建或测试命令；原因：任务为文档生成，且未修改业务代码。
- 已根据上述证据生成根目录 `AGENTS.md`，并保证内容不包含未实现能力或敏感信息。

## 外部信息
- 本轮未使用线上/外部资料。

---

## 追加任务：OpenAI / Claude 端点资料

### OpenAI（可访问公开资料）
- `openai-python` 官方 README 明确：主流接口是 **Responses API**，旧标准接口仍包括 **Chat Completions API**。
- 该 SDK 支持通过 `base_url` 自定义为 `http://.../v1`，并以 `OPENAI_API_KEY` 作为默认密钥来源。
- Azure 形态使用单独的 `AzureOpenAI` 类，参数包括 `azure_endpoint` 和 `api_version`。
- OpenAI 官方 API 参考页在本环境中访问受限（403），因此后续文档应基于可访问的官方 SDK/README 与仓库现有实现来表述，避免引用无法验证的细节。

### Claude / Anthropic（可访问公开资料）
- 官方快速开始文档给出消息接口：`POST https://api.anthropic.com/v1/messages`。
- 认证头示例：`x-api-key: $ANTHROPIC_API_KEY`，并要求 `anthropic-version: 2023-06-01`。
- `messages` 是输入主结构，`max_tokens` 必填/核心参数，系统提示通过顶层 `system` 参数提供，而不是消息中的 `system` role。
- `anthropic-sdk-python` README 说明默认从 `ANTHROPIC_API_KEY` 读取密钥，SDK 直接调用 `client.messages.create(...)`。

### 与本仓库的关联
- 本仓库当前实现的是 **Azure OpenAI 代理**，对外提供 OpenAI 风格的 `/v1/*` 兼容入口，并不直接实现 Claude 网关。
- 因此 `OPENAI.MD` 更适合描述本仓库的 OpenAI-compatible 入口与 Azure OpenAI 相关要求；`CLAUDE.md` 更适合记录 Claude 官方端点与调用要求，避免把两者混写。

### 追加任务落地状态
- `OPENAI.MD`：已写入根目录，内容覆盖端点、鉴权、请求结构、SDK 约定和本仓库注意事项。
- `CLAUDE.md`：已写入根目录，内容覆盖端点、鉴权、消息结构、约束和本仓库适配边界。

---

## 追加任务：请求/回应结构核对

### OpenAI 官方公开资料（可验证来源）
- `openai-python` 的 `src/openai/resources/responses/api.md` 明确 Responses API 端点：`POST /responses`、`GET /responses/{response_id}`、`POST /responses/{response_id}/cancel`、`POST /responses/compact`、`POST /responses/input_tokens`。
- `src/openai/types/responses/response_create_params.py` 明确请求核心字段：`model`、`input`、`instructions`、`max_output_tokens`、`tools`、`tool_choice`、`stream`、`temperature`、`top_p`、`metadata`、`previous_response_id`、`conversation`、`prompt`、`reasoning`、`background`、`store`、`service_tier`、`safety_identifier`、`prompt_cache_key`、`truncation`、`text`、`include` 等。
- `src/openai/types/responses/response.py` 明确响应核心字段：`id`、`object=response`、`created_at`、`status`、`model`、`output[]`、`usage`、`error`、`incomplete_details`、`conversation`、`tool_choice`、`tools`、`background`、`service_tier`、`output_text` 便利属性。
- `src/openai/types/chat/completion_create_params.py` 明确 Chat Completions 请求核心字段：`messages`、`model`、`max_completion_tokens`/`max_tokens`、`temperature`、`top_p`、`tools`、`tool_choice`、`response_format`、`stream`、`n`、`logprobs`、`top_logprobs`、`seed`、`metadata`、`store`、`stop`、`service_tier`、`modalities`、`audio` 等。
- `src/openai/types/chat/chat_completion.py` 明确 Chat Completions 响应字段：`id`、`object=chat.completion`、`created`、`model`、`choices[]`、`usage`、`system_fingerprint`、`service_tier`。

### Claude / Anthropic 官方公开资料（可验证来源）
- `src/anthropic/types/message_create_params.py` 明确 Messages API 端点请求核心字段：`model`、`max_tokens`、`messages`、`system`、`tools`、`tool_choice`、`temperature`、`top_p`、`top_k`、`stop_sequences`、`thinking`、`metadata`、`output_config`、`service_tier`、`stream`、`cache_control`、`container`、`inference_geo`。
- `src/anthropic/types/message.py` 明确响应字段：`id`、`type=message`、`role=assistant`、`content[]`、`model`、`stop_reason`、`stop_sequence`、`usage`、`container`。
- `src/anthropic/types/message_tokens_count.py` 明确 token 计数响应：`input_tokens`。
- `docs.claude.com/en/docs/initial-setup` 公开文档确认 Messages API 端点为 `POST /v1/messages`，认证头为 `x-api-key` 与 `anthropic-version: 2023-06-01`。

### 文档扩写依据
- OpenAI 侧优先以 `responses` 为主，`chat.completions` 作为兼容补充，并对响应对象与流式事件做概览说明。
- Claude 侧以 `messages` 为主，补充 content block、stop_reason 和 token 计数接口。

### 追加任务：Responses 流式事件格式
- `ResponseStreamEvent` 是按 `type` 字段区分的判别式联合类型。
- 常见分组：生命周期（`response.created` / `queued` / `in_progress` / `completed` / `failed` / `incomplete` / `error`）、文本增量、文本完成、输出项、内容块、补充事件（如 `response.refusal.*`、`response.reasoning_text.*`）。
- 文档补充时以 `type` + `sequence_number` 为主线，辅以 `item_id`、`output_index`、`content_index`、`delta`、`text`、`item`、`part` 等关键字段。

### 追加任务落地状态
- `OPENAI.MD`：已扩写为包含 Responses / Chat Completions 的请求与回应结构说明。
- `CLAUDE.md`：已扩写为包含 Messages / count_tokens 的请求与回应结构说明。

---

## 新增改造：配置 NoSQL 化与消费统计

### 方案初判
- 采用文件型文档存储，避免把客户端、模型费用、消费记录继续放进单一 `config.json`。
- 建议拆分为：
  - `config/clients.json`：客户端账户集合（CRUD）；
  - `config/model_costs.json`：模型 token 费用配置；
  - `config/usage_events.jsonl`：消费事件追加日志（按请求记录）。
- 主 `config.json` 仅保留服务、Azure targets、日志以及上述数据文件路径。

### 中间件/采集口径
- 在代理响应写回阶段做 best-effort usage 解析：优先从 JSON body / SSE 事件中提取 usage；若缺失则跳过，不阻断请求。
- 采集字段建议至少包含：`client_name`、`request_id`、`model`、`input_tokens`、`output_tokens`、`cached_tokens`、`status_code`、`target`、`path`、`timestamp`。
- 统计页面可基于 usage 事件按时间窗口聚合；费用按模型费率文件实时计算。

### 网页系统
- 复用现有 `/admin` 鉴权，新增 `/admin/ui` 页面与 `/admin/api/*` JSON 接口。
- 页面建议包含：客户端管理、统计页（30 天柱状图、每小时/昨日消费、时间段总表）、模型费用查看/编辑入口。

### 已落地的后端方案（来自 coder 回传）
- 主配置新增 `data_files`，指向：
  - `clients_file`
  - `model_costs_file`
  - `usage_events_file`
- 新增文件型存储：
  - `config/clients.json`
  - `config/model_costs.json`
  - `config/usage_events.jsonl`
- 新增管理数据接口：
  - 客户端 CRUD：`/admin/data/clients`、`/admin/data/clients/{name}`
  - 模型费用：`/admin/data/model-costs`、`/admin/data/model-costs/{model}`
  - usage/统计：`/admin/data/usage/events`、`/admin/data/usage/aggregate`、`/admin/data/usage/summary`
- usage 采集为 best-effort：在成功响应后尝试从 JSON / SSE 中解析 `input_tokens`、`output_tokens`、`cached_tokens`，缺失时不阻断请求。
- 为提高 SSE 场景鲁棒性，usage 捕获缓冲已改为“保留尾部字节”，避免长流式响应只记录开头而丢失末尾 usage 信息。

### 已落地的网页系统（来自 coder 回传）
- 新增 Admin UI 入口：`GET /admin/` 与 `GET /admin/ui`。
- 通过 `go:embed` 内嵌单页管理界面，页面不依赖外部前端构建工具。
- 页面包含两个主 TAB：
  - 客户端管理：客户端列表、新增/编辑/删除/刷新，模型费用只读表；
  - 消费统计：近 30 天日均柱状图、近 1 小时/昨日/近 30 天统计卡片、时间段内按用户总 token 消费表。
- UI 已通过 admin 鉴权中间件验证，可在 `/admin` 挂载下访问。

### 已同步的文档更新
- `README.md`：已改为说明客户端账户存放在 `config/clients.json` 等 `data_files` 指定位置，并补充 `/admin/ui` 与 `/admin/data/*`。
- `docs/api-contract.md`：已补充 `/admin/ui`、客户端 CRUD、模型费用、usage 统计 API 说明。
- `docs/operations.md`：已补充 `config/` 下数据文件的部署/备份/恢复/权限注意事项，以及消费统计与模型费用运维说明。

### Docker 构建与测试结果
- Docker 镜像构建成功，镜像名：`azure-proxy:test`。
- 容器烟雾测试通过（临时配置目录挂载到 `/etc/azure-proxy`）：
  - `/healthz` 返回 200
  - `/admin/ui` 返回 200
  - `/admin/data/clients` 返回 200

---

## 新任务：后台管理系统设计文档

### 已知背景
- 用户明确反馈：当前“后台”更像 API 列表页，不符合其预期；希望先做设计，再做真正的后台管理系统。

### 当前设计待澄清项
- 登录方式已确认：独立账号密码；不再复用 Bearer token 直接作为后台会话凭据。
- 默认按“后台账号 + 会话（cookie）”来设计，后续可再考虑 token/API-key 兼容入口。
- 是否需要左侧菜单 + 顶部导航的传统后台布局。
- 是否需要总览页（关键指标卡、最近请求、错误率、消费概览）。
- 是否要把客户端管理、模型费用、消费统计作为后台核心一级菜单。
- 是否需要操作审计/变更记录页。

### 已确认的设计前提
- 登录路径建议固定为 `/login`，登录成功后进入 `/admin`。
- 后台会话与客户端访问令牌分离；客户端 token 仅用于代理接口访问，不参与后台登录。
- 后台主导航建议采用传统左侧栏布局，降低学习成本，便于扩展模块。
- 首版后台应优先覆盖：总览、客户端、模型费用、消费统计、审计/日志；其他能力后置。
- 后台账号数据建议采用独立的文件型数据源（例如 `config/admin_users.json`），避免与客户端代理凭据混在一起。
- 会话建议采用 cookie + 签名/过期机制，配合配置中的 session secret，适配单机与容器部署。

### 新任务启动记录
- 已将工作从"设计文档"推进到"实际实现真正后台管理系统"。
- 接下来实现重点：后台登录/会话、后台鉴权与客户端代理鉴权分离、左侧导航式后台 UI、后台账号文件型数据源。

### 后端与 UI 实现已全部落地
- 全部后端新增文件：`users.go`、`session.go`、`audit.go`、`portal.go`、`ui_embed.go`、`ui/index.html`
- 全部改动文件：`handler.go`（新增 auditStore 参数）、`config.go`（AdminSession + DataFiles 扩展）、`main.go`（路由重构）
- 测试全部修复并通过
- 密码哈希格式：`sha256$<salt>$<hex>`，不可逆
- Session 实现：内存 map + HMAC-SHA256 cookie 签名 + sliding expiration

### Docker 烟雾测试关键发现
- 之前失败原因：烟雾测试配置中 `resource_path_prefix` 为空字符串 `""`，触发 `Validate()` 校验 `azure_targets[0] resource_path_prefix must not be empty` 导致进程退出
- 修正为 `/openai` 后，容器正常启动，10 项烟雾测试全部通过
- 路径解析验证：`data_files` 中的相对路径（如 `clients.json`）被正确解析为 `/etc/azure-proxy/clients.json`（基于 config.json 所在目录）
- 权限验证：容器以 root 运行时所有 data_files 可读写；生产部署建议确保 `azureproxy` 用户对挂载目录有写权限

### 内部设计大纲已落地
- 已在 `.codex_plan/reference/admin-backoffice-design.md` 形成内部设计大纲，作为后续实现与评审的唯一骨架参考。
- 大纲明确了登录、页面结构、路由、数据模型、里程碑与暂不做项，避免继续回到 API 面板思路。
- 已补充核心流程：登录、页面加载、配置变更、审计，以及各页面的数据契约。
- 已补充后台 API 约定、账号与权限策略、会话与安全策略、错误反馈规范与实现顺序。

---

## 新任务：bbolt NoSQL 迁移 — 代码现状分析

### 当前 5 个数据存储的实现模式
1. **ClientStore** (`internal/nosql/clients.go`)
   - JSON 文件全量读写 + `writeAtomic`（写 tmp → rename）
   - 方法：`List`、`Create`、`Update`、`Delete`
   - 依赖：`config.Client` 类型
   - 每次写操作先全量读取，修改后全量重写

2. **ModelCostStore** (`internal/nosql/model_costs.go`)
   - JSON 文件全量读写 + `writeAtomic`
   - 方法：`List`、`Upsert`、`Delete`
   - 定义了 `ModelCost` 结构体（在 nosql 包内）
   - `writeAtomic` 辅助函数也在此文件

3. **UsageStore** (`internal/usage/store.go`)
   - JSONL 追加写入 + 全量扫描读取
   - 方法：`Record`、`List`、`Aggregate`、`Summary`
   - 实现了 `Recorder` 接口
   - 大量类型定义在同一文件：Event、Filter、CostTable、Totals、Bucket 等

4. **UserStore** (`internal/admin/users.go`)
   - JSON 文件只读 + 认证逻辑
   - 方法：`List`、`Authenticate`
   - 包含 `HashPassword`、`verifyPasswordHash` 函数
   - 依赖 `config.AdminUser` 类型

5. **AuditStore** (`internal/admin/audit.go`)
   - JSONL 追加写入 + 全量扫描读取
   - 方法：`Record`、`List`
   - 定义了 `AuditEvent` 结构体

### Handler 中的 Store 创建模式（需改造）
- `handler.go` 中 `currentClientStore()`、`currentModelCostStore()`、`currentUsageStore()` 每次请求从配置读取路径创建新 store 实例。
- bbolt 迁移后应改为启动时创建一次，作为 Handler 构造参数传入。

### bbolt 选型依据
- 纯 Go 实现，无 CGO 依赖
- 单文件数据库，事务安全（ACID）
- etcd、Kubernetes 等项目验证
- 适合嵌入式场景，无需独立进程
- 读事务可并行，写事务串行（适合本项目的中低并发写入场景）

### 迁移策略
- bbolt 使用 5 个 bucket：`clients`、`model_costs`、`usage_events`、`admin_users`、`admin_audit`
- 每个 bucket 中以 key-value 存储：
  - `clients`：key=name, value=json(Client)
  - `model_costs`：key=model, value=json(ModelCost)
  - `usage_events`：key=timestamp+uuid, value=json(Event)
  - `admin_users`：key=username, value=json(AdminUser)
  - `admin_audit`：key=timestamp+id, value=json(AuditEvent)
- 配置变更：`data_files`（5 路径）→ `data_store.db_path`（单一路径）
- 启动时检测：若 bbolt DB 不存在但旧 JSON 文件存在，自动迁移
