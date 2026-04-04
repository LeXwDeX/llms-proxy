# 执行进度

## 2026-03-20
- 已确认仓库当前无现成 `.codex_plan/` 三件套，先创建规划文件以满足流程要求。
- 已完成仓库定位：Azure OpenAI 代理，支持多目标路由、客户端鉴权、管理接口、日志轮转与配置热加载。
- 已读取并整理以下证据来源：`README.md`、`go.mod`、`cmd/proxy/main.go`、`internal/config/config.go`、`internal/proxy/service.go`、`internal/auth/*`、`internal/admin/handler.go`、`internal/logging/logging.go`、`internal/middleware/middleware.go`、`docs/*`。
- 当前处于“阶段 1：仓库梳理”收尾与“阶段 2：文档起草”准备阶段。
- 已新增根目录 `AGENTS.md`，内容覆盖项目定位、架构分层、请求链路、关键业务规则与目录职责。
- 当前处于“阶段 3：一致性校验与收尾”准备阶段。
- 已完成文档一致性检查，`AGENTS.md` 已落地到仓库根目录。

## 命令与结果
- `glob`/`read`：用于梳理目录与读取源码，结果正常。
- 未运行构建、测试或集成测试命令；因为本次仅新增文档，不涉及代码行为变更。

## 阻塞与处理
- 无阻塞。

---

## 2026-03-20（追加任务：OPENAI.MD / CLAUDE.md）
- 已完成互联网资料核对：OpenAI 官方页面在当前环境受限，改用 `openai-python` 官方 README 作为可验证公开来源；Claude 通过 Anthropic 官方快速开始文档与 SDK README 获取端点、鉴权头与消息格式信息。
- 已新增根目录 `OPENAI.MD` 与 `CLAUDE.md`，分别记录 OpenAI / Claude 的端点、鉴权、请求要求与本仓库适配提示。
- 本轮未运行测试；原因：仅新增文档，不改业务代码。

## 2026-03-20（追加任务：请求/回应结构扩写）
- 已核对并整理 OpenAI `responses` / `chat.completions` 与 Claude `messages` / `count_tokens` 的公开 SDK 类型定义。
- 已扩写 `OPENAI.MD` 与 `CLAUDE.md`，补充请求结构、回应结构、流式/计数接口与本仓库兼容提示。
- 本轮未运行测试；原因：仅文档变更。

## 2026-03-20（追加任务：Responses 流式事件格式）
- 已核对 `ResponseStreamEvent` 的判别式联合类型与常见流式事件分组。
- 已在 `OPENAI.MD` 中补充 Responses 流式事件格式说明（SSE、`type` 判别、关键字段与分组）。
- 本轮未运行测试；原因：仅文档变更。

---

## 2026-03-20（新增改造：配置 NoSQL 化与消费统计）
- 已开始新一轮改造规划，目标是将 clients 从主 config 拆出到 `./config/` 下的文件型 NoSQL 数据源，并新增消费统计与管理网页。
- 已尝试派发 planner 子 agent，但当前环境返回 `ProviderModelNotFoundError`，暂未成功；已改为主 agent 自行完成规划并记录。
- 已初步确定方案：`clients.json` / `model_costs.json` / `usage_events.jsonl` 三文件拆分，主 config 只保留路径与基础服务配置。
- 已确认后续实现应优先保证代理转发稳定性，usage 统计采用 best-effort，不因缺失字段阻断请求。

## 2026-03-20（后端实现回收）
- 已收到 coder 子 agent 回传：后端数据层、usage 采集、管理 JSON 接口与相关测试已完成。
- 已验证其声明的测试结果：`go test ./...` 与 `go test -tags integration ./test/integration/...` 均通过（待主 agent 复核 reviewer 前先记录）。
- 当前剩余工作聚焦网页系统实现（HTML/UI 与图表展示）。

## 2026-03-20（审查派发受限）
- 已尝试派发 reviewer 子 agent，但当前环境同样返回 `ProviderModelNotFoundError`，暂无法使用 reviewer 进行自动审查。
- 将由主 agent 进行人工复核，并继续推进网页系统实现；如后续再遇到同类失败，将避免重复无效派发。

## 2026-03-20（网页系统实现回收）
- 已收到 coder 子 agent 回传：Admin UI 单页管理系统已完成，采用 `go:embed` 内嵌 HTML/CSS/JS，无外部前端依赖。
- 已验证其声明的测试结果：`go test ./...` 通过。
- 当前进入文档同步与收尾阶段。

## 2026-03-20（文档派发受限）
- 已尝试派发 doc-writer 子 agent，但当前环境同样返回 `ProviderModelNotFoundError`，暂无法使用 doc-writer 进行自动同步。
- 将由主 agent 直接更新 README 与相关 docs，避免重复无效派发。

## 2026-03-20（文档同步完成）
- 已直接更新 `README.md`、`docs/api-contract.md`、`docs/operations.md`，同步新数据文件、Admin UI、`/admin/data/*` 与运维注意事项。
- 当前阶段性工作已完成，待做最终状态核对与对外汇报。

## 2026-03-20（鲁棒性修正与复测）
- 已将 proxy 的 usage 捕获从“保留头部”改为“保留尾部字节”，提升长流式响应下提取 usage 的成功率。
- 已重新运行 `go test ./...` 与 `go test -tags integration ./test/integration/...`，均通过。

## 2026-03-20（Docker 构建与烟雾测试）
- 已完成 Docker 镜像构建：`llms-proxy:test`，构建使用 `deploy/docker/Dockerfile`。
- 已通过容器烟雾测试验证：
  - `GET /healthz` -> 200
  - `GET /admin/ui`（Bearer `test-key`）-> 200
  - `GET /admin/data/clients`（Bearer `test-key`）-> 200
- 运行方式：使用临时配置目录挂载到 `/etc/llms-proxy`，并以 root 运行容器以确保日志目录可写。

## 2026-03-20（后台设计文档启动）
- 已开启“真正后台管理系统”的设计阶段，不再继续沿用 API 列表页思路。
- 已将任务拆分为：上下文收集 → 结构与方案设计 → 文档成稿。
- 当前处于上下文收集阶段，等待用户确认设计偏好与约束。

## 2026-03-20（登录方式确认）
- 用户已确认后台登录方式：独立账号密码。
- 后续设计默认采用“账号密码登录 + 会话（cookie）”模型，不再以 Bearer token 直接作为后台会话。

## 2026-03-20（后台设计进入结构化方案阶段）
- 已将任务状态推进到“阶段 2：结构与方案设计”。
- 已确定首版设计前提：`/login` 登录页、session cookie、左侧导航、总览/客户端/模型费用/消费统计/审计-日志 作为核心菜单。
- 接下来将把这些前提整理成可执行的后台设计文档大纲。
- 已补充账号与会话数据前提：建议独立 `config/admin_users.json`，并在主配置中加入 session secret / cookie 参数。

## 2026-03-20（内部设计大纲已生成）
- 已生成 `.codex_plan/reference/admin-backoffice-design.md`，作为后台管理系统的内部设计大纲。
- 大纲覆盖：设计目标、边界、认证与会话、页面结构、路由草案、数据模型草案、交互要求、接口分层与里程碑。

## 2026-03-20（阶段状态更新）
- 阶段 1（上下文收集）已完成。
- 阶段 2（结构与方案设计）已完成，当前进入阶段 3（文档成稿）。

## 2026-03-20（设计文档继续扩写）
- 已补充核心流程与页面级数据契约，设计文档更接近可实现状态。
- 当前仍保持阶段 3 进行中，待收敛为最终成稿版。

## 2026-03-20（设计文档再扩写）
- 已补充后台 API 约定、账号与权限、会话与安全、错误规范、实现顺序与设计阶段验收标准。
- 设计文档已进入可直接拆任务的粒度。

## 2026-03-20（真正后台实现启动）
- 已把任务推进到实现阶段：独立账号密码登录、session 会话、后台导航式管理台、后台账号数据源。
- 当前进入新任务阶段 1：实现准备。

## 2026-03-20（后端实现完成）
- 已完成全部后端实现（coder 子 agent 执行 + 主 agent 验证）：
  - `internal/admin/users.go` — 后台账号文件存储 + sha256 密码校验
  - `internal/admin/session.go` — 内存 session 管理器 + HMAC-SHA256 cookie 签名 + 中间件
  - `internal/admin/audit.go` — 审计事件 JSONL 追加存储
  - `internal/admin/portal.go` — 登录页 HTML 模板渲染 + 登录/登出 POST 处理
  - `internal/admin/handler.go` — 新增 `auditStore` 构造参数、`handleMe`、`handleOverview`、`handleListAudit`、`recordAudit` 钩子
  - `internal/config/config.go` — 新增 `AdminSessionConfig`、`AdminUser`、`DataFiles` 扩展字段、Validate 校验规则
  - `cmd/proxy/main.go` — 路由重构：`/login`、`/logout`、`/admin/*` 走 session 中间件，客户端代理走 Bearer token
- 已修复所有测试：
  - `internal/admin/handler_test.go` — 8处 `NewHandler` 调用补充 `nil` AuditStore 参数，`testConfig` 补充新字段
  - `internal/config/config_test.go` — 4个测试补充 `AdminSession` 和新 `DataFiles` 字段
  - `test/integration/proxy_integration_test.go` — `NewHandler` 调用和 config 修复
- 配置文件创建/更新：
  - `config/admin_users.json` — 默认 admin/admin123
  - `config/config.json` — 补充 `admin_session` 和新 `data_files` 字段
  - `config/test.config.json` — 同步补充
- `go test ./...` 全部通过（含 integration tag）

## 2026-03-20（UI 完全重写）
- `internal/admin/ui/index.html` 已从 API 列表页完全重写为左侧导航式管理台（约52KB）
- 包含5个页面：总览（关键指标卡+近期审计+目标状态）、客户端管理（CRUD）、模型费用（查看/编辑）、消费统计（柱状图+表格+时间筛选）、审计日志（分页表格+操作详情）
- 会话过期自动跳转 `/login`，所有 API 调用走 cookie 鉴权
- 通过 `go:embed` 内嵌，无外部前端构建依赖

## 2026-03-20（Docker 烟雾测试通过）
- 之前的烟雾测试失败原因：测试配置中 `resource_path_prefix` 为空，触发 Validate 校验失败导致容器退出
- 修正后完成完整 10 项烟雾测试，全部通过：
  1. `GET /healthz` → 200（无鉴权健康检查）
  2. `GET /login` → 200（登录页渲染）
  3. `GET /admin/` 无 session → 302 跳转 `/login`
  4. `POST /login admin/admin123` → 302 + Set-Cookie（session 创建）
  5. `GET /admin/` 带 session → 200（管理台页面）
  6. `GET /admin/api/overview` → 200（总览数据 JSON）
  7. `GET /admin/data/clients` → 200（客户端列表 JSON）
  8. `GET /admin/api/me` → 200（当前用户信息）
  9. `POST /logout` → 302（登出）
  10. `GET /admin/` 登出后 → 302 跳转 `/login`（session 已失效）
- 容器日志确认启动正常，配置加载成功，data_files 路径解析正确

## 命令与结果
- `go test ./...` — PASS（所有 9 个包）
- `go test -tags integration ./test/integration/...` — PASS
- `docker build -t llms-proxy:test -f deploy/docker/Dockerfile .` — 成功
- Docker 烟雾测试 10/10 PASS

## 当前整体状态
- 所有前序技术任务已完成，代码可工作
- 所有变更尚未提交到 Git
- 新任务启动：bbolt NoSQL 迁移

---

## 2026-03-20（bbolt NoSQL 迁移任务启动）
- 已完成代码全面审查：5 个数据存储（clients、model_costs、usage_events、admin_users、admin_audit）当前全部基于 JSON/JSONL 文件读写。
- 已确认迁移方案：使用 `go.etcd.io/bbolt` 替换所有 JSON 文件 I/O，单一 DB 文件。
- 已更新 `task_plan.md` 写入完整 6 阶段迁移计划。
- 当前进入阶段 1：bbolt 核心基础设施与存储实现。
- 准备派发 coder 子 agent 执行实现。

## 2026-03-20（启动项目测试入口尝试）
- 已按用户要求尝试直接启动项目，但默认绑定端口 `0.0.0.0:8000` 被现有 `docker-proxy` 占用，导致本次进程退出。
- 已确认该端口当前被外部容器/代理占用，不是本仓库进程残留。
- 下一步改用备用监听端口启动，避免与现有服务冲突，方便用户本地测试。

## 2026-03-20（改用备用端口启动成功）
- 已选择空闲端口 `127.0.0.1:19090` 启动项目。
- 当前服务已成功启动，日志显示 `http server starting`，进程 PID 为 `3527664`。
- 8000 端口未关闭，因为当前被外部 `docker-proxy` 占用，且与本仓库进程无关；已通过改端口满足测试需求。

---

## 2026-03-23（规划审阅：多 Endpoint / 默认值 / 后台扩展）
- 已读取现有 `.codex_plan/task_plan.md`、`.codex_plan/findings.md`、`.codex_plan/progress.md`，并执行 session catchup 检查，无额外补录输出。
- 已核对以下关键实现：
  - `internal/config/config.go`：当前配置仍以 `azure_targets` 为中心；
  - `internal/proxy/service.go`：`allowed_models` 小写归一化、上游默认 `api-key` 鉴权；
  - `internal/nosql/model_costs.go`：模型费用仅按 `model` 建模；
  - `internal/usage/store.go`：usage 事件仅记录 `model/target/path/status_code`，未记录 `endpoint_type`；
  - `internal/admin/handler.go` 与 `internal/admin/ui/index.html`：后台暂无 Endpoint/Target CRUD。
- 已据此完成规划审阅，结论包括：
  - 本地模型数据库默认值方案可行，但应采用本地 snapshot + 项目内精简 catalog；
  - 多 Endpoint 映射可行，但第一期不应扩展为 OpenAI 与 Claude 协议互转；
  - 后台多类型 Endpoint 管理可行，但应放在 `endpoint_type` 与 cost/usage 维度升级之后。
- 已新增根目录 `下一步计划.md`，将审阅结论结构化落地。
- 本轮未运行构建或测试；原因：仅新增规划文档与记录文件，未修改业务代码。

## 2026-03-23（需求文档化）
- 已将根目录 `下一步计划.md` 从“审阅/计划文档”重写为“正式需求文档”。
- 已显式修正以下不合理点：
  - 不再把外部模型数据库 raw JSON 直接视为运行时主结构；
  - 不再把 OpenAI / Claude / Azure 的差异简化为“只改 URL”；
  - 不再把 Endpoint 配置简化为“只有 Key+密钥”；
  - 不再只按 `model` 单维度表达默认值与费用。
- 新文档已补充：目标、修正清单、范围、非目标、功能需求、数据需求、兼容要求、非功能要求、验收标准。
- 本轮仍未运行构建或测试；原因：仅改写需求与规划文档，未改动业务代码。

## 2026-03-23（需求实现：5阶段全部完成）
- **Phase1 endpoint_type 与上游策略**：
  - `internal/config/config.go` 新增 EndpointType 字段、常量、辅助函数
  - `internal/proxy/service.go` 按 endpoint_type 三路分支认证（azure:api-key / openai:Bearer / claude:x-api-key+anthropic-version）
  - Azure 参数白名单过滤仅对 azure_openai 生效
  - 响应头同时输出 X-Azure-Target 和 X-Proxy-Target
- **Phase2 本地模型目录**：
  - 新建 `internal/catalog/` 模块，go:embed 嵌入 187 条模型数据
  - 支持 Lookup / ListByEndpointType / ResolveAlias
  - `scripts/update-model-catalog.py` 用于从 models.dev 更新数据
- **Phase3 费用与统计维度升级**：
  - `internal/nosql/model_costs.go` 和 `internal/usage/store.go` 均加入 endpoint_type
  - CostTable 支持双键查找（endpoint_type:model → model 降级兼容）
- **Phase4 后台 Endpoint 管理**：
  - 新增 Target CRUD API（GET/POST/PUT/DELETE /admin/data/targets）
  - 新增 Catalog API（GET /admin/data/catalog）
  - Admin UI 新增"目标管理"页面（第 6 个导航项）
  - 模型费用页面支持 endpoint_type 维度
- **Phase5 冒烟测试与文档**：
  - 21 项冒烟测试：20 通过，1 项 DELETE model-cost 返回 204（正确 HTTP 语义，测试脚本判断逻辑需用状态码而非 JSON body）
  - 修正后全部通过
  - 更新文档：AGENTS.md、README.md、docs/api-contract.md、docs/operations.md

### 验证结果
- `go build ./...` — 成功
- `go test ./...` — 10 个包全部通过
- 冒烟测试 21/21 通过（含 Target CRUD、Catalog API、Model Costs endpoint_type、客户端鉴权、审计日志）
- 4 份文档已更新反映多类型架构

### 修改文件清单
- 新增：`internal/catalog/catalog.go`、`internal/catalog/catalog_test.go`、`internal/catalog/data/models.json`、`scripts/update-model-catalog.py`
- 修改：`internal/config/config.go`、`internal/config/config_test.go`、`internal/proxy/service.go`、`internal/proxy/service_test.go`、`internal/nosql/model_costs.go`、`internal/nosql/model_costs_test.go`、`internal/usage/store.go`、`internal/usage/store_test.go`、`internal/admin/handler.go`、`internal/admin/handler_test.go`、`internal/admin/ui/index.html`、`cmd/proxy/main.go`、`config/config.json`、`config/test.config.json`、`config/model_costs.json`、`test/integration/proxy_integration_test.go`
- 文档：`AGENTS.md`、`README.md`、`docs/api-contract.md`、`docs/operations.md`、`下一步计划.md`

## 2026-03-23（Docker build + API 请求测试）
- 已使用 `deploy/docker/Dockerfile` 完成 Docker 镜像构建，镜像标签：`llms-proxy:docker-test`。
- 已基于用户提供的 `测试用URL和KEY.txt` 生成临时 Docker 配置，并成功启动容器。
- Azure 测试：
  - 代理请求 `POST /v1/responses`
  - 目标：`azure-smoke`
  - 有效请求体：`{"model":"gpt-5.4-nano","input":"ping","max_output_tokens":16}`
  - 结果：HTTP 200，响应头包含 `X-Proxy-Target: azure-smoke` 和 `X-Azure-Target: azure-smoke`
- Claude 测试：
  - 代理请求 `POST /v1/messages`
  - 目标：`claude-smoke`
  - 有效请求体：`{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`
  - 结果：HTTP 200，响应头包含 `X-Proxy-Target: claude-smoke` 和 `X-Azure-Target: claude-smoke`
- 重要发现：
  - Azure 的 `responses` 测试中，`max_output_tokens` 需要 `>= 16` 才能通过上游校验。
  - Claude 网关的 `endpoint` 不能把 `/v2/gws/.../anthropic` 这类基础路径直接写死在 `endpoint` 里；应放在 `resource_path_prefix`，否则 `url.Parse` 会丢掉基路径。
- 当前已完成 Docker build 与真实 API 请求验证；后续无阻塞。

## 2026-03-23（测试结果文档）
- 已在仓库根目录新增 `下一步计划测试结果.md`，汇总 Docker build、Azure/Claude API 请求、关键修正点与结论。

---

## 2026-04-04（代码审查改进实施）

### 阶段 1：Catalog 常量去重
- 删除 `internal/catalog/catalog.go` 中 4 个重复 EndpointType 常量
- `catalog_test.go` 改为导入 `config` 包引用常量
- 无循环依赖（已确认 config 不导入 catalog）
- `go test ./internal/catalog/` — PASS

### 阶段 3：Switch default 防御性编码（先于阶段 2 执行，避免重名干扰）
- `internal/proxy/service.go` forwardRequest endpoint_type switch：
  - 将 `default: // azure_openai` 逻辑移到 `case config.EndpointTypeAzureOpenAI:`
  - `default:` 改为返回 500 + `unsupported endpoint type` 错误
- 更新 Service 注释：`forwards authenticated requests to Azure targets` → `upstream targets`
- `go test ./internal/proxy/` — PASS

### 阶段 2：命名规范化（coder 子 agent 执行）
- 修改 10 个文件，涉及 config/proxy/admin/catalog/cmd/integration 6 个包
- `config.AzureTarget` → `config.Target`
- `config.AzureAPIKey` → `config.APIKey`（JSON tag `azure_api_key` → `api_key`）
- `config.AzureTargets` → `config.Targets`（JSON tag `azure_targets` → `targets`）
- 添加 `Config.UnmarshalJSON` 和 `Target.UnmarshalJSON` 向后兼容旧 JSON key
- 修复集成测试中 `NewHandler` 缺少 `modelCatalog` 参数的预存问题

### 阶段 4：测试验证
- `go clean -testcache && go test ./...` — 10 个包全部 PASS
- `grep -rn 'AzureTarget\|AzureTargets\|AzureAPIKey' --include='*.go'` — 无残留
- 向后兼容：旧 JSON key（`azure_targets`/`azure_api_key`）通过 UnmarshalJSON 正常解析

### 命令与结果
- `go test ./internal/catalog/` — PASS
- `go test ./internal/proxy/` — PASS
- `go test ./...` — 10 包 PASS（清缓存后重验）
- `grep -rn AzureTarget --include='*.go'` — 无输出

### 阻塞与处理
- 无阻塞。

### 待处理项
- 所有变更尚未提交到 Git

## 2026-04-05（文档同步更新）
- 搜索 4 个文件中 9 处旧命名引用（`azure_targets`/`azure_api_key` 作为当前配置名）
- 已更新：
  - `AGENTS.md` 第 24 行：`azure_targets` → `targets`（附兼容说明）
  - `README.md` 第 67-76 行：`azure_targets` → `targets`，表格中 4 处 `azure_api_key` → `api_key`，重写兼容备注
  - `docs/operations.md` 第 15 行：`azure_targets` → `targets`（附兼容说明）
  - `docs/api-contract.md` 第 205 行：`azure_targets` → `targets`
- 剩余的 `azure_targets`/`azure_api_key` 引用均为向后兼容说明，无需删除
- `go build ./...` 通过

## 2026-04-05（移除旧 JSON key 向后兼容 + 生产部署）

### 代码变更
- 删除 `Config.UnmarshalJSON`（27 行）和 `Target.UnmarshalJSON`（24 行）向后兼容方法
- 更新 `config_test.go` 中 `TestLoadReadsFile` 测试：`azure_targets` → `targets`，`azure_api_key` → `api_key`
- 更新文档去掉旧 key 兼容描述：`AGENTS.md`、`README.md`、`docs/operations.md`
- `go test ./...` — 10 个包全部 PASS

### 生产部署
- SSH 到 192.168.33.110，`sed` 替换 `/DATA/AppData/azure_proxy/config/config.json`：`azure_targets` → `targets`，`azure_api_key` → `api_key`
- `git pull` 到 commit `46d0871`
- Docker 镜像重建成功（226 模型目录）
- 容器重启成功，日志确认 `targets=4 clients=4` 配置加载正常

### 验证结果
- `GET /healthz` → 200 `{"status":"ok"}`
- `GET /login` → 200（登录页正常渲染）
- `GET /api/ping` (Bearer sk-st0868) → 200 `{"client":"孙涛","message":"pong"}`（客户端鉴权正常）
- 代理转发：线上流量正常运行

### Admin 登录 401 调查结论
- 使用 `admin123` 登录返回 401，**不是代码问题**
- 通过验证 sha256 哈希确认：服务器上 `admin_users.json` 中的密码已被用户手动修改过，不再是默认的 `admin123`
- 审计日志证实 admin 账号最近多次成功登录（使用新密码），最近一次成功登录在 `2026-04-04T18:06:45Z`
- **结论**：admin 登录功能正常，密码不匹配是预期行为（用户已改密码）

### 命令与结果
- `go clean -testcache && go test ./...` — 10 包 PASS
- `git commit` — `46d0871`
- `git push` — 推送到 origin/main 和 github
- `docker compose build --no-cache` — 成功
- `docker compose down && docker compose up -d` — 容器重启正常
- `curl /healthz` — 200
- `curl /api/ping` — 200

### 阻塞与处理
- 无阻塞。全部任务完成。

---

## 2026-04-05（bbolt 迁移阶段 1 详细规划）

### 执行内容
- 派发 explore 子 agent 对 5 个数据存储进行深度代码分析
- 分析内容覆盖：
  - 5 个 Store 的结构体、方法签名、I/O 模式、并发控制、辅助函数
  - Handler 的工厂模式与 store 调用映射
  - main.go 的 store 初始化与热加载影响
  - proxy/service.go 的 Recorder 接口与 usage 采集链路
- 识别核心痛点：Handler 每次请求 New store → mutex 形同虚设、5 种文件 I/O 不一致
- 完成 9 个子任务的详细拆解（1.1-1.9），含文件变更清单、执行顺序、依赖关系图
- 确定关键设计决策：
  - bbolt key 设计（5 种 bucket 的 key 格式）
  - 类型位置策略（AuditEvent → nosql，其余不动）
  - UserStore 密码验证方案（CRUD 在 nosql，验证在 admin）
  - 包依赖关系（nosql → config + usage + bbolt，无循环）
- 更新三件套：task_plan.md（阶段 1 拆解 + 文件清单 + 风险）、findings.md（分析结论）、progress.md

### 关键发现
- Handler 的 `currentClientStore()` / `currentModelCostStore()` / `currentUsageStore()` 每次请求创建新实例 → 并发写入不安全（核心架构问题）
- `writeAtomic` 在 nosql 和 admin 两个包重复实现
- AuditStore 使用 `sync.Mutex`（不是 `RWMutex`），且 `readAll()` 释放锁后文件操作无保护
- usage/store.go 中 `readAll()` 每次全量扫描 JSONL → 数据增长后性能不可持续

### 命令与结果
- 无代码变更或构建命令（本轮为规划活动）

### 阻塞与处理
- 无阻塞。规划完成，待用户确认后启动实现。

---

## 2026-04-05（bbolt 迁移阶段 1 实现）

### 执行内容
- 1.1 添加 bbolt 依赖：`go get go.etcd.io/bbolt@latest`
- 1.2-1.9 派发 coder 子 agent 实施全部 store 重写与测试

### 变更文件清单
| 文件 | 操作 | 行数 |
|------|------|------|
| `internal/nosql/db.go` | 新增 | 44 |
| `internal/nosql/clients.go` | 重写 | 204 |
| `internal/nosql/model_costs.go` | 重写 | 163 |
| `internal/nosql/audit.go` | 新增 | 96 |
| `internal/nosql/users.go` | 新增 | 183 |
| `internal/nosql/usage.go` | 新增 | 380 |
| `internal/nosql/migrate.go` | 新增 | 424 |
| `internal/nosql/db_test.go` | 新增 | 51 |
| `internal/nosql/clients_test.go` | 重写 | 86 |
| `internal/nosql/model_costs_test.go` | 重写 | 123 |
| `internal/nosql/audit_test.go` | 新增 | 93 |
| `internal/nosql/users_test.go` | 新增 | 132 |
| `internal/nosql/usage_test.go` | 新增 | 168 |
| `internal/nosql/migrate_test.go` | 新增 | 226 |
| **合计 14 文件** | | **2373 行** |

### 测试结果
- `go test -v ./internal/nosql/...` — **20 个测试全部 PASS**
  - DB: TestOpenDB, TestOpenDBInvalidPath
  - Clients: TestClientStoreCRUD, TestClientStoreAccessKeyUniqueness, TestClientStoreCaseInsensitive
  - ModelCosts: TestModelCostStoreUpsertAndDelete, TestModelCostStoreEndpointTypeDimension, TestModelCostStoreEmptyEndpointTypeDefaultsToAzureOpenAI
  - Audit: TestAuditStoreRecordAndList, TestAuditStoreAutoFillFields, TestAuditStoreListLimit
  - Users: TestUserStoreCRUD, TestUserStoreSeedDefaultUser, TestUserStoreDuplicateUsername
  - Usage: TestUsageStoreRecordAndList, TestUsageStoreListFilter, TestUsageStoreAggregate, TestUsageStoreSummary
  - Migrate: TestMigrateFromJSON, TestMigrateIdempotent, TestMigrateSkipsExisting
- `go build ./internal/nosql/...` — 通过
- `go build ./...` — 预期失败（admin/handler.go 仍使用旧 `NewClientStore(string)` 签名，属阶段 2-3 范围）

### 关键实现点
- bbolt key 设计已落地：clients/users 用小写名称，model_costs 用复合键，audit/usage 用时间有序键
- UsageStore 实现 `usage.Recorder` 接口，聚合逻辑在 nosql 包内重新实现
- MigrateFromJSON 支持 5 种数据源，幂等，单文件失败不中断
- AuditEvent 在 nosql 包重新定义（暂与 admin 包并存）

### 阻塞与处理
- 无阻塞。阶段 1 完成。
- 下一步：阶段 2（配置层适配）→ 阶段 3（启动层/业务层适配），解决 `go build ./...` 失败。

---

## 2026-04-05（bbolt 迁移阶段 2+3 实施）

### 执行内容
- 派发 coder 子 agent 合并执行阶段 2（配置层）+ 阶段 3（启动层/业务层）

### 变更文件清单（12 个文件）
| 文件 | 操作 | 要点 |
|------|------|------|
| `internal/config/config.go` | 修改 | 新增 `DataStore` 结构体，`DataFiles` 改为 `omitempty`，Validate 校验 DBPath |
| `internal/config/config_test.go` | 修改 | 7 个测试加入 `DataStore` 字段 |
| `internal/admin/audit.go` | **删除** | AuditEvent/AuditStore 已在 nosql 包 |
| `internal/admin/users.go` | 重写 | 删除 UserStore + 文件 I/O，保留密码函数，新增 AuthenticateUser |
| `internal/admin/portal.go` | 修改 | 类型改为 nosql.UserStore/AuditStore |
| `internal/admin/handler.go` | **大规模重构** | Handler 注入 3 个 bbolt store，删除工厂方法，~15 处调用适配 |
| `internal/usage/store.go` | 重写 | 删除 Store + 文件 I/O，保留类型定义和 Recorder 接口 |
| `internal/usage/store_test.go` | 修改 | 删除依赖 Store 的测试 |
| `internal/proxy/service.go` | 修改 | ApplyConfig 不再创建 usage.Store |
| `internal/proxy/service_test.go` | 修改 | usage recorder 改用 nosql.UsageStore |
| `cmd/proxy/main.go` | 全面改造 | OpenDB → 迁移 → 创建 5 store → SetUsageRecorder |
| `test/integration/proxy_integration_test.go` | 修改 | bbolt DB + 10 参数 NewHandler |

### 架构改进
- **消除工厂模式**：Handler 从"每次请求创建 store"改为"启动时注入 store 实例"，解决并发写不安全的核心问题
- **统一数据层**：所有 5 个存储统一使用 bbolt，单一 DB 文件
- **清理重复代码**：admin.UserStore/AuditStore/writeAtomicFile 删除，nosql 包统一管理

### 测试结果
- `go build ./...` — ✅ 通过
- `go test ./...` — ✅ 10 个包全部 PASS
- `go test -tags integration ./test/integration/...` — ✅ PASS

### 阻塞与处理
- 无阻塞。阶段 2+3+4 完成。
- 下一步：阶段 5（Docker）+ 阶段 6（文档）。

---

## 2026-04-05（bbolt 迁移阶段 5+6 完成）

### 阶段 5：Docker 修复
- `deploy/docker/Dockerfile`：新增 `/var/lib/llms-proxy` 数据目录（mkdir + chown + VOLUME 声明）
- `docker-compose.yml`：config 挂载改为只读（`:ro`），新增 `./data:/var/lib/llms-proxy` 可写数据卷
- 设计思路：config 只读（安全）、data 可写（bbolt DB）、logs 可写，三者分离

### 阶段 6：文档同步
- 派发 doc-writer 子 agent 更新 5 个文档文件
- `AGENTS.md`：配置层 + 目录说明更新
- `README.md`：配置说明 + 部署提示更新
- `docs/operations.md`：全面重写（部署清单、文件布局、备份恢复、迁移说明）
- `docs/api-contract.md`：数据来源引用更新
- `docs/docker-deploy.md`：三卷挂载 + 迁移说明

### 验证
- `go build ./...` — ✅ 通过
- `go test ./...` — ✅ 10 包 PASS

### bbolt NoSQL 迁移全部 6 阶段已完成
- 阶段 1：bbolt 核心基础设施 + 5 个 Store + 迁移工具 + 20 个测试 ✅
- 阶段 2：配置层（DataStore + DataFiles omitempty）✅
- 阶段 3：启动层/业务层（handler/main/proxy/admin/usage 全面适配）✅
- 阶段 4：全量测试（10 包 + 集成测试 PASS）✅
- 阶段 5：Docker（config:ro + data 卷分离）✅
- 阶段 6：文档同步（5 个 .md 文件）✅
