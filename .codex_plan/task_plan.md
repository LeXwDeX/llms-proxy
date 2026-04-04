# 任务计划：生成根目录 AGENTS.md（架构与业务说明）

## 目标
- 在仓库根目录新增 `AGENTS.md`，用中文清晰描述本项目的架构、业务定位、关键请求流与目录职责。
- 文档内容必须与现有代码、README 和 `docs/` 保持一致，避免引入未实现的能力描述。

## 阶段拆解
### 阶段 1：仓库梳理
- 读取 `README.md`、`go.mod`、核心 `internal/*` 与 `docs/*` 文档，确认项目定位、入口、配置模型、请求链路和运维能力。
- 输出：`findings.md` 记录关键证据。

### 阶段 2：文档起草
- 依据已确认事实，编写根目录 `AGENTS.md`，重点覆盖：
  - 项目定位与业务场景
  - 架构分层与模块职责
  - 请求处理链路与鉴权/路由/回源/管理接口
  - 配置结构与运行约束
  - 目录结构与维护建议

### 阶段 3：一致性校验与收尾
- 检查文档是否与现有实现一致，是否包含敏感信息或虚构能力。
- 输出：`progress.md` 记录执行结果；确认 `AGENTS.md` 已写入根目录。

## 验收门禁
- `AGENTS.md` 存在于仓库根目录。
- 文档包含架构、业务、请求流、目录职责等核心信息。
- 不包含凭据、令牌、环境特定秘密或未实现功能。
- 文档表述与代码事实一致，可由 README / 源码交叉验证。

## 风险与回流条件
- **风险 1：** 文档与代码不一致。回流到阶段 1 复核源码与 README。
- **风险 2：** 过度细化造成“描述了未实现能力”。回流到阶段 2 删减或改写。
- **风险 3：** 忽略运维/管理链路。回流补充 `admin`、`logging`、`middleware` 相关说明。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete
- 阶段 4：complete

---

# 追加任务：移除旧 JSON key 向后兼容 + 生产部署

## 目标
- 移除 `Config.UnmarshalJSON` 和 `Target.UnmarshalJSON` 中旧 JSON key（`azure_targets`/`azure_api_key`）的向后兼容代码。
- 更新生产服务器配置文件，直接使用新 key（`targets`/`api_key`）。
- 重建 Docker 镜像并部署到生产。
- 调查 admin 登录 401 问题。

## 阶段拆解

### 阶段 1：代码清理
- 删除 `Config.UnmarshalJSON` 和 `Target.UnmarshalJSON` 方法
- 更新测试用例中的旧 JSON key
- 更新文档中的旧 key 兼容描述
- `go test ./...` 全部通过

### 阶段 2：生产配置更新
- SSH 到 192.168.33.110 更新 `config.json`
- `azure_targets` → `targets`，`azure_api_key` → `api_key`

### 阶段 3：部署
- 提交并推送代码
- 服务器 git pull + Docker 重建 + 容器重启

### 阶段 4：验证
- healthz + API ping + admin 登录调查

## 验收门禁
- `go test ./...` 全部通过
- 代码中不存在旧 JSON key 向后兼容逻辑
- 生产配置使用新 key，服务正常运行
- admin 登录问题根因已查明

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete
- 阶段 4：complete

---

# 追加任务：客户端配置 NoSQL 化、消费统计与网页管理系统

## 目标
- 将 `clients` 从主 `config.json` 拆分为位于 `./config/` 的 NoSQL 数据文件，并保留热加载/校验能力。
- 增加消费统计中间件：按客户端 `name` 汇总上传总 token、回传总 token、缓存命中数，且对缺失字段/非标准响应具备鲁棒性。
- 新增网页管理系统：基于 `./config/` 的 NoSQL 数据源，实现客户端账户的增删改查，并提供每小时、昨日、近 30 天等消费统计视图。
- 引入模型 token 费用配置，支持在统计页上推算大致消费金额。

## 阶段拆解
### 阶段 1：方案与数据结构设计
- 明确 NoSQL 文件格式、数据迁移方式、统计口径、网页路由与页面结构。
- 输出：`findings.md` 记录方案结论与约束。

### 阶段 2：后端实现
- 重构配置与客户端存储层。
- 添加 token 消费中间件与统计聚合器。
- 增加模型费用配置读取与汇总计算。
- 提供客户端管理与统计的 HTTP 接口。

### 阶段 3：网页系统实现
- 制作管理网页与统计 TAB 页，展示 CRUD 与统计图表/表格。
- 确保页面与 `./config/` 数据源一致。

### 阶段 4：测试、回归与文档
- 补充/更新测试与操作文档，验证配置读写、统计口径与网页可用性。

## 验收门禁
- `clients` 不再存放在主 `config.json`，而是由 `./config/` 下的 NoSQL 文件提供。
- 可以通过网页增删改查客户端账户。
- 可以查看至少近 30 天的日均消费柱状图与时间段内用户总 token 表格。
- 可以按模型费用配置推算大致消费金额。
- 中间件对缺失 usage 字段、流式响应、异常响应具备容错。

## 风险与回流条件
- **风险 1：** 数据格式设计不稳定。回流阶段 1 先定 schema 与迁移方案。
- **风险 2：** Token 统计口径与上游返回不一致。回流阶段 2 调整中间件与解析逻辑。
- **风险 3：** 页面统计粒度/图表需求变化。回流阶段 3 调整页面与接口。
- **风险 4：** 现有代理转发行为被破坏。回流阶段 2 优先保持现有路由与转发兼容。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete
- 阶段 4：complete

---

# 新任务：后台管理系统设计文档

## 目标
- 先形成一份可执行的设计文档，明确“真正的后台管理系统”应该长什么样，再进入实现。
- 重点解决：浏览器可直接访问的后台登录/会话、左侧菜单、总览页、客户端管理页、模型费用管理页、消费统计页，以及对应的鉴权和数据流。

## 阶段拆解
### 阶段 1：上下文收集
- 明确文档类型、目标读者、预期效果、格式要求、现有约束与优先级。
- 输出：`findings.md` 记录需求澄清点。

### 阶段 2：结构与方案设计
- 产出后台系统的信息架构、页面结构、路由分层、交互流程、权限模型与数据接口草案。
- 明确哪些内容必须先做，哪些可后置。

### 阶段 3：文档成稿
- 将设计内容整理为可执行文档，确保读者能据此实现或评审。

## 验收门禁
- 文档明确描述“后台管理系统”而不是普通 API 列表页。
- 文档包含登录/会话、导航、页面、数据流、接口、权限与非功能要求。
- 文档可作为后续实现依据，不依赖口头说明。

## 风险与回流条件
- **风险 1：** 需求边界不清。回流阶段 1 重新澄清。
- **风险 2：** 设计落回“API 面板”。回流阶段 2 补齐浏览器端交互与会话层。
- **风险 3：** 文档过长。回流阶段 3 收敛为“必须项 / 可选项”。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete

---

# 新任务：代码审查 + 发布部署

## 目标
- 审查当前未提交变更（middleware SSE Flush/Unwrap 修复），确认代码质量无问题。
- 整体审查近期代码，确保无回归风险。
- 编译 linux-amd64 二进制并部署到生产服务器。
- **关键约束**：服务器 `/etc/llms-proxy/` 配置目录包含运行时数据，绝对不能覆盖。

## 阶段拆解

### 阶段 1：代码审查
- Review `internal/middleware/middleware.go` 的 Flush/Unwrap 变更。
- 整体审查近期代码质量（最近几次提交的 Gemini 支持等）。
- 运行 `go test ./...` 确保测试通过。

### 阶段 2：编译与提交
- 提交 middleware 修复变更。
- 交叉编译 `GOOS=linux GOARCH=amd64` 二进制。

### 阶段 3：部署
- SSH 到服务器。
- 备份旧二进制 `/opt/llms-proxy/bin/llms-proxy`。
- 上传新二进制（SCP/rsync），**仅替换二进制文件**。
- **禁止操作** `/etc/llms-proxy/` 下的任何配置或数据文件。
- 重启 systemd 服务 `systemctl restart llms-proxy`。

### 阶段 4：验证
- 检查服务启动日志 `journalctl -u llms-proxy`。
- 验证 `/healthz` 端点可达。
- 验证后台登录与基本功能正常。

## 验收门禁
- `go test ./...` 全部通过。
- linux-amd64 二进制编译成功。
- 服务器上服务正常运行，`/healthz` 返回 200。
- 服务器上 `/etc/llms-proxy/` 配置与数据文件未被修改。

## 风险与回流条件
- **风险 1：** middleware 变更引入回归。回流阶段 1 修复。
- **风险 2：** 部署后服务无法启动。回滚到旧二进制。
- **风险 3：** 配置目录被意外覆盖。部署步骤严格限制为仅操作二进制。

## 当前状态
- 阶段 1：complete（代码审查完成，改进已实施为独立追加任务）
- 阶段 2：pending
- 阶段 3：pending
- 阶段 4：pending

## 当前设计方向（已确认）
- 后台登录采用**独立账号密码**，与客户端代理 token 解耦。
- 默认使用 **session cookie** 维持后台登录态。
- 后台定位为“真正的管理台”，而不是 API 列表页或接口测试台。

## 阶段 2：结构与方案设计（细化）
### 2.1 认证与会话
- 登录页 `/login`。
- 账号密码登录后建立服务端 session。
- 支持退出登录、会话过期、未登录跳转。
- 后台鉴权中间件与客户端代理鉴权分离。

### 2.2 页面结构
- 左侧导航 + 顶部状态栏。
- 一级菜单建议：
  - 总览
  - 客户端管理
  - 模型费用
  - 消费统计
  - 审计/日志

### 2.3 数据与接口
- 后台账号数据源独立于客户端数据源，建议采用 `config/admin_users.json`。
- 配置中补充后台会话参数（cookie 名称、签名密钥、过期时间）。
- 需要明确：账号、角色、密码哈希、登录态、操作审计。
- 页面所需接口按“页面域”划分，而不是按“单个按钮”散落设计。

### 2.4 非功能要求
- 浏览器直达、无需外部构建链。
- 与现有代理/统计能力共存，不破坏当前 `/admin/data/*` 能力。
- 为后续 RBAC、审计、账号管理留扩展点。

### 2.5 设计成果
- 以上内容已固化为 `.codex_plan/reference/admin-backoffice-design.md`，后续实现/评审以该大纲为准。

---

# 追加任务：补充 `OPENAI.MD` 的 Responses 流式事件格式

## 目标
- 在 `OPENAI.MD` 中补充 Responses API 的流式事件格式，说明 SSE 事件分组、`type` 判别字段、常见事件名与关键字段。
- 说明应基于官方公开 SDK 类型定义，避免引入不实字段。

## 阶段拆解
### 阶段 1：资料核对
- 核对 OpenAI Responses 流式事件的公开类型定义，整理事件分组与关键字段。
- 输出：`findings.md` 记录来源与结论。

### 阶段 2：文档补充
- 在 `OPENAI.MD` 中新增“流式事件格式”章节，按生命周期、文本、输出项、内容块与补充事件分组说明。

### 阶段 3：收尾
- 检查 `OPENAI.MD` 表述清晰且与公开资料一致；记录进度。

## 验收门禁
- `OPENAI.MD` 包含 Responses 流式事件格式说明。
- 说明中包含 `type` 判别、常见事件名和关键字段。
- 不写入未经确认的私有字段。

## 风险与回流条件
- **风险 1：** 事件过多导致文档冗长。回流到阶段 2 只保留高频事件与分组说明。
- **风险 2：** 字段名与 SDK 类型不一致。回流到阶段 1 重新核对。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete

---

# 追加任务：扩写 `OPENAI.MD` 与 `CLAUDE.md` 的请求/回应结构

## 目标
- 将 `OPENAI.MD` 与 `CLAUDE.md` 扩写为包含“请求结构”和“回应结构”的完整说明，覆盖主端点、常用参数、返回对象、流式/计数接口与本仓库注意事项。
- 说明需依据可验证的官方 SDK / 文档，不写入未经确认的字段或行为。

## 阶段拆解
### 阶段 1：结构资料核对
- 核对 OpenAI Responses / Chat Completions 与 Claude Messages 的官方 SDK 类型定义和公开文档。
- 输出：`findings.md` 记录来源与关键字段。

### 阶段 2：文档扩写
- 扩写 `OPENAI.MD`：补充 Responses API、Chat Completions 的请求结构与响应结构，以及流式与计数接口概览。
- 扩写 `CLAUDE.MD`：补充 Messages API 的请求结构、内容块、响应结构、流式与 token 计数接口。

### 阶段 3：校验与收尾
- 确认两份文档可独立阅读且与公开资料一致；记录结果到 `progress.md`。

## 验收门禁
- `OPENAI.MD` 与 `CLAUDE.MD` 均包含请求结构与回应结构说明。
- 文档列出的字段、对象和端点可由官方公开资料交叉验证。
- 不引入凭据、密钥或未经确认的私有细节。

## 风险与回流条件
- **风险 1：** 字段过多导致失焦。回流阶段 2 以“主字段 + 常用可选字段 + 典型返回对象”重写。
- **风险 2：** 官方文档版本差异。回流阶段 1 以 SDK 类型定义为准。
- **风险 3：** 与本仓库代理行为冲突。回流阶段 2 增补本仓库兼容提示。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete

---

# 追加任务：生成 `OPENAI.MD` 与 `CLAUDE.md`（端点与要求说明）

## 目标
- 在仓库根目录新增 `OPENAI.MD` 与 `CLAUDE.md`，分别记录 OpenAI / Claude 的官方端点、鉴权头、请求格式与使用要求。
- 文档需结合互联网公开资料与本仓库现有代理行为，避免写入不准确或不适用的内容。

## 阶段拆解
### 阶段 1：互联网与仓库信息核对
- 查阅 OpenAI / Anthropic 官方公开文档，以及本仓库中与 OpenAI-compatible / Azure OpenAI 相关的实现与说明。
- 输出：`findings.md` 记录可验证来源与关键结论。

### 阶段 2：文档编写
- 生成 `OPENAI.MD`：聚焦 OpenAI 的常用端点、认证、请求字段、流式与错误约定，以及本仓库相关的兼容注意事项。
- 生成 `CLAUDE.md`：聚焦 Claude（Anthropic）的消息接口端点、认证头、模型与消息结构要求、流式与限制。

### 阶段 3：一致性校验与收尾
- 检查两份文档是否准确、精炼、无敏感信息。
- 输出：`progress.md` 记录完成情况，并确认根目录文件已落地。

## 验收门禁
- 根目录存在 `OPENAI.MD` 与 `CLAUDE.md`。
- 内容包含端点、鉴权、请求格式、关键限制或注意事项。
- 与公开文档和仓库实际行为一致，不写入凭据或臆测内容。

## 风险与回流条件
- **风险 1：** 公开文档与仓库行为不一致。回流阶段 1 重新核对。
- **风险 2：** OpenAI 官方页面受限或不可访问。回流使用可访问的官方 SDK/README/公开文档作为依据，并在 findings 记录来源。
- **风险 3：** 文档过长或过泛。回流阶段 2 精简为端点与要求摘要。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete

---

# 新任务：将 JSON 文件存储迁移为 bbolt 嵌入式 NoSQL 数据库

## 目标
- 将 `internal/nosql/` 包从"假 NoSQL（JSON 文件读写）"改为真正的嵌入式 NoSQL 数据库（bbolt）。
- 将分散在 `internal/usage/store.go`、`internal/admin/users.go`、`internal/admin/audit.go` 中的文件 I/O 统一到 `internal/nosql/` 包。
- 配置从 `data_files`（5 个路径）改为 `data_store`（单一 DB 路径）。
- 修复 Docker 部署中 read-only 文件系统导致所有 CRUD 失败的问题。
- 提供启动时自动从 JSON 文件迁移到 bbolt 的能力。

## 阶段拆解

### 阶段 1：bbolt 核心基础设施与全部存储实现（详细拆解）

#### 1.1 添加 bbolt 依赖
- `go get go.etcd.io/bbolt`
- 确认 `go build ./...` 通过
- 验收：`go.mod` 和 `go.sum` 包含 bbolt 依赖

#### 1.2 DB 管理层 — 新增 `internal/nosql/db.go`
- 定义 5 个 bucket 常量：`BucketClients`, `BucketModelCosts`, `BucketUsageEvents`, `BucketAdminUsers`, `BucketAdminAudit`, `BucketMeta`
- `OpenDB(path string) (*bbolt.DB, error)`：打开 bbolt 文件，创建所有 bucket
- `CloseDB(db *bbolt.DB) error`：关闭 DB
- bbolt 选项：`Timeout: 1s`（避免启动时死锁），`NoSync: false`（保证持久性）
- 验收：可创建 DB 文件，打开后所有 bucket 存在

#### 1.3 重写 ClientStore — 修改 `internal/nosql/clients.go`
- 构造函数：`NewClientStore(db *bbolt.DB) *ClientStore`（不再接收 path）
- 结构体字段：`db *bbolt.DB`（不再需要 `mu sync.RWMutex` 和 `path string`）
- 删除 `Path()`, `SetPath()` 方法
- 保持公开 API 不变：`List`, `Create`, `Update`, `Delete`
- Key = `{name}`，Value = `json.Marshal(config.Client)`
- 读操作使用 `db.View()`，写操作使用 `db.Update()`
- 删除所有文件 I/O 函数：`readClients`, `writeClients`, `ensureJSONFile`
- `writeAtomic` 函数暂保留（ModelCostStore 也在用），最终在 1.4 一起删除
- 验收：`List/Create/Update/Delete` 操作 bbolt，不再操作文件系统
- 单元测试：使用 `t.TempDir()` + `OpenDB()` 临时 DB

#### 1.4 重写 ModelCostStore — 修改 `internal/nosql/model_costs.go`
- 构造函数：`NewModelCostStore(db *bbolt.DB) *ModelCostStore`
- 结构体字段：`db *bbolt.DB`
- 删除 `Path()`, `SetPath()` 方法
- 保持公开 API 不变：`List`, `Upsert`, `Delete`, `DeleteByKey`
- Key = `{endpoint_type}:{model}`（复合键），Value = `json.Marshal(ModelCost)`
- Delete（按 model 删除全部 endpoint_type）：遍历 bucket 删除所有后缀匹配 `:{model}` 的 key
- 删除所有文件 I/O 函数：`readModelCosts`, `writeModelCosts`, `writeAtomic`, `ensureJSONFile`
- 验收：同 1.3
- 单元测试

#### 1.5 新增 AuditStore — 新增 `internal/nosql/audit.go`
- 将 `AuditEvent` 类型定义从 `internal/admin/audit.go` 迁移到 `internal/nosql/audit.go`
- 构造函数：`NewAuditStore(db *bbolt.DB) *AuditStore`
- 结构体字段：`db *bbolt.DB`
- 公开 API：`Record(event AuditEvent) error`, `List(limit int) ([]AuditEvent, error)`
- Key = `{RFC3339Nano}_{uuid}`，Value = `json.Marshal(AuditEvent)`
- `Record`：如果 ID 为空则自动生成 uuid，如果 Timestamp 为零则取 now
- `List`：使用 Cursor 反向遍历（`Last()` → `Prev()`），取前 N 条
- 验收：追加与倒序查询正确
- 单元测试

#### 1.6 新增 UserStore — 新增 `internal/nosql/users.go`
- 构造函数：`NewUserStore(db *bbolt.DB) *UserStore`
- 结构体字段：`db *bbolt.DB`
- 公开 API：
  - `List() ([]config.AdminUser, error)` — 遍历 bucket
  - `Get(username string) (config.AdminUser, error)` — 按 key 精确查找
  - `Create(user config.AdminUser) error` — 校验 + 写入
  - `Update(username string, user config.AdminUser) error` — 覆盖写
  - `Delete(username string) error`
  - `SeedDefaultUser(user config.AdminUser) error` — bucket 为空时写入（幂等）
- Key = `{username}`，Value = `json.Marshal(config.AdminUser)`
- **不包含密码验证逻辑**（`HashPassword`/`verifyPasswordHash` 保留在 admin 包）
- admin 包保留 `Authenticate(store *nosql.UserStore, username, password string)` 包级函数
- 验收：CRUD + Seed 正确
- 单元测试

#### 1.7 新增 UsageStore — 新增 `internal/nosql/usage.go`
- **这是最复杂的 store**，需要实现 `usage.Recorder` 接口
- 构造函数：`NewUsageStore(db *bbolt.DB) *UsageStore`
- 结构体字段：`db *bbolt.DB`
- 公开 API（完全对齐现有 `usage.Store`）：
  - `Record(event usage.Event) error` — 实现 `usage.Recorder` 接口
  - `List(filter usage.Filter) ([]usage.Event, error)` — 按时间范围+客户端+模型过滤
  - `Aggregate(filter usage.Filter, groupBy string, costs usage.CostTable) (usage.AggregateResult, error)`
  - `Summary(now time.Time, costs usage.CostTable) (usage.SummaryResult, error)`
- Key = `{RFC3339Nano}_{uuid}`
- 查询优化：利用 Cursor.Seek 定位时间范围起点，避免全量扫描
- 包依赖：`nosql` 导入 `usage` 获取类型和接口（不产生循环）
- `internal/usage/store.go` 中保留：类型定义（Event, Filter, CostTable, Totals 等）、接口（Recorder）、辅助函数（bucketStartFor, normalizeGroupBy, estimateEventCost 等）；删除：Store 结构体和所有文件 I/O 方法
- 验收：Record/List/Aggregate/Summary 全部正确，且实现 `usage.Recorder` 接口
- 单元测试

#### 1.8 迁移工具 — 新增 `internal/nosql/migrate.go`
- `MigrateFromJSON(db *bbolt.DB, dataFiles config.DataFiles) error`
- 迁移流程：
  1. 检查 `meta` bucket 中是否有 `migrated` 标记 → 有则跳过
  2. 逐个检查旧 JSON 文件是否存在
  3. 对每个存在的文件：读取 → 写入对应 bucket
  4. 全部完成后在 `meta` bucket 写入 `{"migrated_at":"...", "source":"json_files"}`
- 旧文件不删除（保留备份）
- 单次事务完成所有迁移（保证原子性），如果数据量过大则按 bucket 分事务
- 容错：单个文件迁移失败不影响其他文件，记录 warning 继续
- 验收：从旧 JSON 文件成功迁移到 bbolt，迁移后数据完整，重复执行幂等
- 单元测试

#### 1.9 单元测试 — 新增/修改 `internal/nosql/*_test.go`
- `db_test.go`：OpenDB/CloseDB/bucket 存在性
- `clients_test.go`：改为 bbolt 版 CRUD 测试
- `model_costs_test.go`：改为 bbolt 版 CRUD 测试
- `audit_test.go`：Record + List 倒序
- `users_test.go`：CRUD + SeedDefaultUser 幂等性
- `usage_test.go`：Record + List + Aggregate + Summary + 时间范围查询
- `migrate_test.go`：JSON→bbolt 迁移 + 幂等 + 容错
- 每个测试使用 `t.TempDir()` 创建独立 DB，测试间完全隔离
- 验收：`go test ./internal/nosql/...` 全部通过

### 阶段 2：配置层适配
- `internal/config/config.go`：`DataFiles`（5 个路径字段）→ `DataStore`（`DBPath` 单一字段 + 可选 `MigrationDir` 指定旧 JSON 文件目录）。
- `internal/config/config_test.go`：同步更新所有配置测试。
- `config/config.json` + `config/test.config.json`：更新格式。

### 阶段 3：启动层与业务层适配
- `cmd/proxy/main.go`：启动时打开 bbolt DB，创建所有 store，执行迁移检测，传递给 handler。
- `internal/admin/handler.go`：Handler 接收 store 实例而非每次请求从配置创建。去掉 `currentClientStore()`/`currentModelCostStore()`/`currentUsageStore()` 模式。
- `internal/admin/portal.go`：适配新 UserStore 来源。
- `internal/admin/users.go`：保留密码哈希/校验函数，删除文件 I/O 逻辑。
- `internal/admin/audit.go`：删除（已移入 nosql 包）。
- `internal/usage/store.go`：保留类型定义（Event、Filter、CostTable 等），删除文件 I/O 逻辑。
- `internal/proxy/service.go`：usage 采集适配新 Recorder 接口。

### 阶段 4：测试
- `internal/nosql/*_test.go`：为所有 bbolt store 编写单元测试。
- `internal/admin/handler_test.go`：适配新构造签名。
- `internal/config/config_test.go`：适配新配置结构。
- `test/integration/proxy_integration_test.go`：适配。
- `go test ./...` 全部通过。

### 阶段 5：Docker 修复与回归
- 更新 `deploy/docker/Dockerfile`：声明数据卷。
- 更新 `docker-compose.yml`：config 只读挂载 + data 可写卷分离。
- Docker 烟雾测试通过。
- 全部容器日志错误已修复。

### 阶段 6：文档同步
- 更新 README、api-contract、operations 中的配置说明。
- 更新 AGENTS.md 中的架构说明。

## 验收门禁
- `go test ./...` 全部通过。
- Docker 容器中所有 CRUD 操作正常（config 只读 + data 可写）。
- 容器日志无 read-only filesystem、favicon 401、audit 文件缺失等错误。
- 启动时自动检测并迁移旧 JSON 文件到 bbolt。
- 现有代理转发功能不退化。
- 浏览器可正常登录后台并执行客户端/模型费用/消费统计/审计的所有操作。

## 风险与回流条件
- **风险 1：** bbolt 事务/并发模型不匹配。回流阶段 1 调整 store 实现。
- **风险 2：** 配置变更影响现有部署。回流阶段 2 调整配置兼容策略。
- **风险 3：** 迁移逻辑丢失数据。回流阶段 1 补充迁移前备份与校验。
- **风险 4：** Handler 签名变更导致大面积测试失败。回流阶段 4 逐步修复。
- **风险 5：** Docker 部署权限问题。回流阶段 5 调整用户/卷设置。

## 当前状态
- 阶段 1：complete（9 个子任务全部完成，20 个测试 PASS）
  - 1.1 添加 bbolt 依赖：complete
  - 1.2 DB 管理层 db.go：complete
  - 1.3 重写 ClientStore：complete
  - 1.4 重写 ModelCostStore：complete
  - 1.5 新增 AuditStore（含 AuditEvent 迁移）：complete
  - 1.6 新增 UserStore：complete
  - 1.7 新增 UsageStore（最复杂）：complete
  - 1.8 迁移工具 migrate.go：complete
  - 1.9 单元测试：complete
- 阶段 2：complete（DataStore 配置结构 + DataFiles omitempty 迁移兼容）
- 阶段 3：complete（handler/main/proxy/admin/usage 全面适配）
- 阶段 4：complete（go test ./... 10 包 PASS + 集成测试 PASS）
- 阶段 5：complete（Dockerfile 新增 /var/lib/llms-proxy 数据目录，docker-compose config:ro + data 卷分离）
- 阶段 6：complete（AGENTS.md/README.md/operations.md/api-contract.md/docker-deploy.md 全部更新）

## 阶段 1 文件变更清单

### 新增文件
| 文件 | 说明 |
|------|------|
| `internal/nosql/db.go` | bbolt DB 管理层（OpenDB/CloseDB/bucket 常量） |
| `internal/nosql/audit.go` | AuditStore（bbolt）+ AuditEvent 类型定义 |
| `internal/nosql/users.go` | UserStore（bbolt）CRUD |
| `internal/nosql/usage.go` | UsageStore（bbolt）实现 usage.Recorder |
| `internal/nosql/migrate.go` | JSON→bbolt 自动迁移工具 |
| `internal/nosql/db_test.go` | DB 管理层测试 |
| `internal/nosql/audit_test.go` | AuditStore 测试 |
| `internal/nosql/users_test.go` | UserStore 测试 |
| `internal/nosql/usage_test.go` | UsageStore 测试 |
| `internal/nosql/migrate_test.go` | 迁移测试 |

### 修改文件
| 文件 | 变更 |
|------|------|
| `go.mod` / `go.sum` | 添加 bbolt 依赖 |
| `internal/nosql/clients.go` | 重写为 bbolt（删除文件 I/O） |
| `internal/nosql/model_costs.go` | 重写为 bbolt（删除文件 I/O + writeAtomic） |
| `internal/nosql/clients_test.go` | 适配 bbolt 版构造函数 |
| `internal/nosql/model_costs_test.go` | 适配 bbolt 版构造函数 |

### 阶段 1 不动的文件（后续阶段处理）
| 文件 | 何时处理 | 原因 |
|------|---------|------|
| `internal/admin/handler.go` | 阶段 3 | 需要改构造函数和去工厂模式 |
| `internal/admin/portal.go` | 阶段 3 | 适配新 UserStore 来源 |
| `internal/admin/users.go` | 阶段 3 | 删除文件 I/O，保留密码函数 |
| `internal/admin/audit.go` | 阶段 3 | 删除（逻辑已迁移到 nosql） |
| `internal/usage/store.go` | 阶段 3 | 删除 Store/文件 I/O，保留类型定义 |
| `internal/proxy/service.go` | 阶段 3 | 适配新 Recorder |
| `internal/config/config.go` | 阶段 2 | 配置结构变更 |
| `cmd/proxy/main.go` | 阶段 3 | 启动时初始化 bbolt DB |

## 阶段 1 执行顺序与依赖

```
1.1 go get bbolt
 │
 ▼
1.2 db.go（DB 管理层）
 │
 ├──▶ 1.3 clients.go（最简 CRUD，验证模式）
 │
 ├──▶ 1.4 model_costs.go（复合 key CRUD）
 │
 ├──▶ 1.5 audit.go（AuditEvent 迁移 + 追加式存储）
 │
 ├──▶ 1.6 users.go（CRUD + Seed）
 │
 └──▶ 1.7 usage.go（最复杂：Recorder + Aggregate）
      │
      ▼
     1.8 migrate.go（依赖所有 store）
      │
      ▼
     1.9 单元测试（每个子任务附带测试，最后统一验证）
```

1.3-1.6 可并行开发（无相互依赖），1.7 建议在 1.3 验证模式后再做。

## 阶段 1 验收门禁
- `go test ./internal/nosql/...` 全部通过
- 每个 store 的 CRUD 测试覆盖正常路径、边界情况和错误路径
- 迁移工具可从旧 JSON 文件完整迁移数据到 bbolt
- 迁移幂等：重复执行不报错、不重复数据
- 无循环导入：`nosql → config`✓、`nosql → usage`✓、`nosql → bbolt`✓
- `go build ./...` 通过（即使旧代码暂未适配，nosql 包自身可编译）

## 阶段 1 风险
- **风险 1**：bbolt 在 WSL/Docker 环境下的 mmap 行为。缓解：CI 中跑 `go test`。
- **风险 2**：UsageStore 全量遍历 bucket 性能。缓解：Cursor.Seek 按时间前缀定位。
- **风险 3**：AuditEvent 类型迁移导致 admin 包编译失败。缓解：阶段 1 期间 admin 包可暂时 import nosql.AuditEvent（admin 已依赖 nosql），但阶段 3 才真正清理旧代码。
- **风险 4**：usage 包的辅助函数（filterEvents, bucketStartFor 等）在 nosql/usage.go 中需要使用。缓解：先以 nosql 包内重新实现或将辅助函数导出到 usage 包。

---

# 追加任务：审阅多 Endpoint / 默认值扩展规划并输出下一步计划

## 目标
- 结合当前仓库实现，审阅“本地模型数据库默认值、多类型 Endpoint 映射、后台多类型 Endpoint 管理”三部分规划的合理性、可行性与优化空间。
- 将结论整理为仓库根目录 `下一步计划.md`，供后续实现拆分与评审使用。

## 阶段拆解
### 阶段 1：现状核对
- 阅读 `config`、`proxy`、`admin`、`usage`、`nosql`、`README` 与既有 `.codex_plan` 记录。
- 明确当前实现边界：配置结构、上游鉴权、模型匹配、后台页面与管理 API 能力。

### 阶段 2：规划审阅
- 对三部分需求逐项给出：合理性、可行性、主要障碍、风险与优化建议。
- 收敛第一期实现边界，避免把“URL 变化”误判为“协议完全兼容”。

### 阶段 3：文档落地
- 新增根目录 `下一步计划.md`，形成结构化的实施建议、阶段顺序、数据结构建议、一期不做项与验收门禁。
- 同步更新 `.codex_plan/findings.md` 与 `.codex_plan/progress.md`。

## 验收门禁
- `下一步计划.md` 存在于仓库根目录。
- 文档明确覆盖：合理性、可行性、优化建议、分阶段实施顺序、涉及模块与风险。
- 审阅意见与代码现状一致，不引入未核实能力。

## 风险与回流条件
- **风险 1：** 忽略当前实现边界，形成无法执行的空泛计划。回流阶段 1 继续补证据。
- **风险 2：** 把 Claude 适配误扩展为协议转换。回流阶段 2 收紧范围到原生协议透传。
- **风险 3：** 后台能力评估不准。回流阶段 1 核对现有 UI/API 是否真的支持 Endpoint 管理。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete

---

# 追加任务：代码审查改进 — 命名规范化 + 防御性编码 + 常量去重

## 目标
- 基于代码审查结论，实施三项改进：
  1. 消除 catalog 包中重复的 EndpointType 常量
  2. 将 Azure 历史命名规范化为通用命名（保持 JSON 向后兼容）
  3. 对 endpoint_type switch 增加防御性编码

## 阶段拆解

### 阶段 1：Catalog 常量去重
- 删除 `internal/catalog/catalog.go` 中重复定义的 4 个 EndpointType 常量
- 改为从 `internal/config` 包导入使用
- 确认无循环依赖

### 阶段 2：命名规范化
- `config.AzureTarget` → `config.Target`
- `config.AzureTarget.AzureAPIKey` → `config.Target.APIKey`，JSON tag `azure_api_key` → `api_key`
- `config.Config.AzureTargets` → `config.Config.Targets`，JSON tag `azure_targets` → `targets`
- 添加 `Config.UnmarshalJSON` 向后兼容旧 JSON key
- 更新所有引用文件与注释

### 阶段 3：Switch default 防御性编码
- 将 azure_openai 逻辑从 `default:` 移到 `case config.EndpointTypeAzureOpenAI:`
- `default:` 返回错误"unsupported endpoint type"

### 阶段 4：测试验证
- `go test ./...` 全部通过
- 确认旧 JSON key 向后兼容

## 验收门禁
- `go test ./...` 全部通过
- catalog 包改为导入 config 包的常量
- 不再出现 `AzureTarget`/`AzureTargets`/`AzureAPIKey` 等旧命名
- switch default 对未知 endpoint_type 返回错误
- 旧 JSON key 仍可正常解析

## 风险与回流条件
- **风险 1：** 循环导入。概率极低。
- **风险 2：** 大规模重命名遗漏。回流阶段 2 补充。
- **风险 3：** JSON 向后兼容不完整。回流阶段 2 完善。
- **风险 4：** switch default 变更拒绝合法请求。回流阶段 3 检查。

## 当前状态
- 阶段 1：complete
- 阶段 2：complete
- 阶段 3：complete
- 阶段 4：complete
