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
- Docker 镜像构建成功，镜像名：`llms-proxy:test`。
- 容器烟雾测试通过（临时配置目录挂载到 `/etc/llms-proxy`）：
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
- 路径解析验证：`data_files` 中的相对路径（如 `clients.json`）被正确解析为 `/etc/llms-proxy/clients.json`（基于 config.json 所在目录）
- 权限验证：容器以 root 运行时所有 data_files 可读写；生产部署建议确保 `llmsproxy` 用户对挂载目录有写权限

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

---

## 追加任务：多 Endpoint / 默认值规划审阅

### 当前实现边界
- 配置层当前只有 `azure_targets` 与 `AzureTarget`，没有通用 `endpoint_type` 抽象；运行时 `buildTargetStates` 也直接接收 `[]config.AzureTarget`。
- `allowed_models` 在构建 target 状态时会做 `strings.ToLower(strings.TrimSpace(...))` 归一化；usage 记录与 model cost 也统一按小写模型名处理。
- 上游请求转发当前默认注入 `api-key`，只有 `allow_bearer_passthrough=true` 且客户端显式传入 `X-Azure-Authorization` 时，才改为上游 `Authorization`。
- 后台当前只有客户端、模型费用、消费统计、审计相关 API 与页面；没有 Endpoint/Target CRUD，也没有 Endpoint 类型下拉或模型-Endpoint 绑定配置页。

### 对用户规划的核心判断
- “把 models 数据库缓存到本地并作为默认值来源”是最容易落地的一项，但不应直接把原始外部数据结构塞进运行时；更稳妥的做法是保留本地 snapshot，并提炼出项目内稳定 catalog schema。
- “新增类 OpenAI / 类 Claude 的 Endpoint 映射”可行，但不能只把差异理解为 URL；至少还涉及上游鉴权头、特定协议字段、列表接口与请求清洗逻辑分支。
- 若支持 Claude，第一期应限定为“Claude 原生协议 -> Claude 官方端点透传”，不做 OpenAI<->Claude 协议互转。
- 模型费用与默认值若继续只按 `model` 建模，会在多 Endpoint 类型并存时产生歧义；至少应升级为 `endpoint_type + model` 维度。
- 后台部分的真实起点不是“把 Azure 类型下拉改成多类型”，而是先补齐 Endpoint/Target 的管理能力，因为当前后台尚无该模块。

### 推荐的一期收敛策略
- 引入 `endpoint_type`，先支持：`azure_openai`、`openai`、`claude` 三种类型。
- 保留 `azure_targets` 配置顶层与现有行为兼容，第一阶段只增字段，不急于全量重命名为更通用的 `targets`。
- 模型默认值采用“本地 vendored snapshot + 项目精简 catalog + 后台手工覆盖”的三层结构。
- 成本与 usage 统计补充 `endpoint_type` 维度后，再推进后台多类型 Endpoint 页面，避免先做 UI 后改底层 schema。

---

## 追加任务：需求文档化修正点

### 已修正的不合理表述
- “把 `models.dev/api.json` 直接融入项目”已收敛为：引入本地快照，并转换为项目内稳定 schema，运行时不依赖外部 URL。
- “类 OpenAI / 类 Claude 除了 URL 之外先都保持一致”已收敛为：第一阶段仅支持同类官方协议透传，按类型处理鉴权头与必要协议头，不做 OpenAI 与 Claude 协议互转。
- “后台中每个 Endpoint 基本就是 Key+密钥”已收敛为：最小必填项至少包括名称、类型、Endpoint、认证信息，必要时带类型专属字段。
- “模型名统一小写后直接匹配默认值”已收敛为：归一化主键至少为 `endpoint_type + normalized_model`，必要时支持 alias。

### 需求文档的正式化方向
- 文档结构已从“审阅意见”切换为“正式需求”。
- 已增加范围、非目标、功能需求、数据需求、兼容性要求、非功能要求与验收标准。
- 已明确默认值只是默认值，不能覆盖手工配置，也不能被当成真实账单。

---

## Docker build / API 请求测试发现

### 构建结果
- 使用 `deploy/docker/Dockerfile` 构建 Docker 镜像成功。
- 镜像标签：`llms-proxy:docker-test`。

### Azure 上游请求
- 有效请求路径：`POST /v1/responses`
- 有效请求体：`{"model":"gpt-5.4-nano","input":"ping","max_output_tokens":16}`
- 返回结果：HTTP 200。
- 需要注意：`max_output_tokens` 低于 16 时，上游会返回校验错误。

### Claude 上游请求
- 有效请求路径：`POST /v1/messages`
- 有效请求体：`{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`
- 返回结果：HTTP 200。
- 需要注意：Claude gateway 的 base path 不能直接写入 `endpoint`，应放在 `resource_path_prefix` 中；否则 `buildURL()` 使用 `url.Parse()` 时会把基路径覆盖掉，导致 404。

### 代理返回头
- 两个成功请求都返回了：
  - `X-Proxy-Target`
  - `X-Azure-Target`

### 测试结论
- Docker build 成功。
- Azure 与 Claude 的代理请求均成功打通。
- 当前没有发现需要回流到代码层的额外问题。

---

## 代码审查改进：命名规范化 + 防御性编码 + 常量去重

### 审查发现（已实施修复）
1. **Catalog 常量去重**：`internal/catalog/catalog.go` 第 20-26 行重复定义了 4 个 EndpointType 常量，与 `internal/config/config.go` 完全相同。已删除冗余定义，改为注释指向 config 包。测试文件改为导入 config 包引用常量。
2. **命名规范化**：
   - `config.AzureTarget` → `config.Target`（Go 类型名）
   - `config.AzureTarget.AzureAPIKey` → `config.Target.APIKey`（Go 字段名 + JSON tag `api_key`）
   - `config.Config.AzureTargets` → `config.Config.Targets`（Go 字段名 + JSON tag `targets`）
   - 通过 `Config.UnmarshalJSON` 和 `Target.UnmarshalJSON` 自定义反序列化，向后兼容旧 JSON key（`azure_targets`/`azure_api_key`）
   - 使用 type-alias 技术避免 UnmarshalJSON 递归调用
3. **Switch default 防御性编码**：`proxy/service.go` forwardRequest 中 endpoint_type switch 从 `default: // azure_openai` 改为显式 `case config.EndpointTypeAzureOpenAI:`，`default:` 返回 500 错误 `unsupported endpoint type`。

### 集成测试修复
- `test/integration/proxy_integration_test.go` 中 `admin.NewHandler` 调用缺少 `modelCatalog` 参数（预存问题），已补充 `nil` 修复。

### 影响范围
- 改动涉及 10 个 Go 源文件，跨 config/proxy/admin/catalog/cmd/integration 6 个包
- 旧 JSON 配置文件（使用 `azure_targets`/`azure_api_key`）仍可正常加载，无破坏性变更
- Go 源码中不再存在任何 `AzureTarget`/`AzureTargets`/`AzureAPIKey` 标识符
- `AGENTS.md` 等文档中仍有 `azure_targets` 描述，后续需同步更新

---

## bbolt NoSQL 迁移 — 阶段 1 详细分析

### 现有 5 个存储的实现模式总结

| Store | 位置 | 类型 | I/O 模式 | 锁类型 | 生命周期 |
|-------|------|------|----------|--------|---------|
| ClientStore | nosql/clients.go | JSON 数组 | 全量读写 | RWMutex | 短命（每次请求 New） |
| ModelCostStore | nosql/model_costs.go | JSON 数组 | 全量读写 | RWMutex | 短命（每次请求 New） |
| UsageStore | usage/store.go | JSONL | 追加写+全量读 | RWMutex | 中长（ApplyConfig 替换） |
| UserStore | admin/users.go | JSON 数组 | 全量读写 | RWMutex | 长命（启动创建一次） |
| AuditStore | admin/audit.go | JSONL | 追加写+全量读 | Mutex | 长命（启动创建一次） |

### Handler 工厂模式问题（核心痛点）

Handler 通过 `currentClientStore()`/`currentModelCostStore()`/`currentUsageStore()` 每次请求创建新 store 实例：
- 各实例 mutex 互不相关 → **并发写入完全没有保护**
- store 实例请求结束后丢弃 → 缓存策略无法生效
- AuditStore 和 UserStore 是长生命周期的（启动时创建一次） → 不对称架构

### writeAtomic 重复实现

- `nosql.writeAtomic(path, payload)` 定义在 model_costs.go，被 clients.go 共享
- `admin.writeAtomicFile(path, data)` 在 users.go 独立实现
- 两者逻辑完全相同：写临时文件 → uuid 命名 → os.Rename

### 包依赖关系（现状）

```
cmd/proxy/main.go → config, auth, proxy, admin, nosql, usage, catalog
admin → config, auth, proxy, nosql, usage, catalog
proxy → config, auth, usage
nosql → config (Client, AdminUser 类型), uuid
usage → (无内部依赖)
```

### bbolt 迁移后的包依赖关系（目标）

```
cmd/proxy/main.go → config, auth, proxy, admin, nosql, usage, catalog
admin → config, auth, proxy, nosql, usage, catalog (admin 保留密码验证)
proxy → config, auth, usage (不变)
nosql → config, usage, bbolt, uuid (新增 usage 和 bbolt 依赖)
usage → (不变，仅保留类型定义和接口)
```

### 关键设计决策

1. **所有 bbolt store 接收 `*bbolt.DB`**，不再接收文件路径
2. **5 个 bucket**：`clients`, `model_costs`, `usage_events`, `admin_users`, `admin_audit`
3. **不再需要 mutex**：bbolt 内部事务安全（并发读 + 串行写）
4. **不再需要 Path/SetPath**：热加载不影响 DB 路径
5. **时间有序数据的 key**：`RFC3339Nano_uuid`，利用 bbolt B+tree 有序性支持时间范围查询

### Key 设计

| Bucket | Key 格式 | 示例 |
|--------|---------|------|
| clients | `{name}` | `"孙涛"` |
| model_costs | `{endpoint_type}:{model}` | `"azure_openai:gpt-5.4-nano"` |
| usage_events | `{RFC3339Nano}_{uuid}` | `"2026-04-05T18:10:53.799Z_f47ac10b..."` |
| admin_users | `{username}` | `"admin"` |
| admin_audit | `{RFC3339Nano}_{uuid}` | `"2026-04-05T18:10:53.799Z_a1b2c3d4..."` |

### 类型位置决策

| 类型 | 当前位置 | 目标位置 | 原因 |
|------|---------|---------|------|
| ModelCost | nosql | nosql（不变） | 已在正确位置 |
| AuditEvent | admin | nosql | 避免 nosql → admin 循环依赖 |
| Event, Filter, CostTable, Totals... | usage | usage（不变） | proxy 已依赖 usage 接口 |
| Recorder 接口 | usage | usage（不变） | proxy 通过接口解耦 |
| AdminUser | config | config（不变） | 多处引用 |
| Client | config | config（不变） | 多处引用 |
| HashPassword 等 | admin/users.go | admin（不变） | 非存储逻辑 |

### UserStore 密码验证方案

- nosql/users.go 只提供 CRUD：`List`, `Get(username)`, `Create`, `Update`, `Delete`, `SeedDefaultUser`
- 密码验证逻辑保留在 admin 包（`HashPassword`, `verifyPasswordHash`）
- admin 包提供上层 `Authenticate(username, password)` 函数，内部调用 `nosql.UserStore.Get()` + `verifyPasswordHash()`

### 迁移策略

1. 启动时检测 bbolt DB 是否存在且已初始化
2. 如果 DB 各 bucket 为空但旧 JSON 文件存在 → 执行迁移
3. 迁移完成后在 `meta` bucket 写入 `{"migrated_at": "...", "source": "json"}`
4. 不删除旧 JSON 文件（保留作为备份）
5. 迁移过程幂等：已有数据则跳过
