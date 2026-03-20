# Azure OpenAI 代理

本项目提供一个轻量级的 HTTP 代理，用于统一转发多个 Azure OpenAI 终端。代理负责集中管理凭据、请求路由、日志，以及基本的故障切换能力，使内部客户端在同一入口下即可访问全部 Azure 资源。

## 功能特性
- 基于 JSON 的配置文件，并支持管理接口触发热加载。
- 按客户端令牌划分访问权限，可限定允许访问的 Azure 目标；客户端账户已拆分为 `./config/` 下的文件型 NoSQL 数据。
- 使用结构化日志，支持文件轮转，兼顾控制台输出。
- 具备智能目标选择机制，遇到网络错误会重试并触发静默窗口。
- 管理接口提供健康检查、指标统计、配置重载，以及网页化管理台与消费统计页。
- 自带单元测试与集成测试脚本，便于本地验证。
- 对外提供 OpenAI 风格 API 入口（`/v1/*`），内部自动转发到 Azure OpenAI。
- 转发遵循 Azure v1 最新规范：不依赖 `api-version`，并自动剥离客户端传入的 `api-version` 参数。

## 目录结构
```
cmd/proxy/           # 应用入口
config/              # 配置样例与模板
internal/            # 核心模块（auth、config、proxy、middleware、logging、admin）
logs/                # 默认日志目录（已预留轮转能力）
scripts/             # 辅助脚本
test/integration/    # 集成测试（使用 -tags integration 运行）
```

## 环境要求
- Go 1.24.2 及以上版本。
- 可用的 Azure OpenAI 终端与 API Key。
- 能够为内部客户端生成并分发访问令牌。

## 快速上手
1. 调整配置：
   打开 `config/config.json` 并根据实际环境修改终端、API Key 及客户端信息。
2. 构建二进制：
   ```sh
   make build
   ```
   可执行文件将生成在 `bin/azure-proxy`。
3. 启动服务：
   ```sh
   ./bin/azure-proxy -config config/config.json
   ```
4. 使用客户端令牌发起请求（与 Azure 调用方式一致，只更换 key 值）：
   ```sh
   # 任意一种即可：
   curl -H "api-key: <client-access-key>" \
        http://localhost:8080/v1/chat/completions
   # 或
   curl -H "Authorization: Bearer <client-access-key>" \
        http://localhost:8080/v1/chat/completions
   ```

## 配置说明
`config/config.json` 中的关键字段：

- `server`：监听地址、对外基址、超时时间、请求体大小限制。
- `azure_targets`：Azure 终端列表及对应 API Key，顺序决定主备优先级；`allowed_models` 用于模型级路由和白名单。
- `data_files`：文件型 NoSQL 数据路径，默认指向 `config/clients.json`、`config/model_costs.json`、`config/usage_events.jsonl`、`config/admin_users.json`、`config/admin_audit.jsonl`；客户端访问令牌、模型费用、消费事件、后台管理员账号与审计日志分别存放在这些文件中。
- `admin_session`：后台管理会话配置，包括 cookie 名称、签名密钥（`secret`）、会话有效期（`ttl_seconds`）、滑动过期（`sliding_expiration`）与安全 cookie 标志。
- `logging`：日志等级及文件路径，轮转策略由 `internal/logging` 统一处理。

请求转发行为（关键点）：
- 客户端只需按 OpenAI API 调用代理（例如 `POST /v1/chat/completions`、`POST /v1/embeddings`、`POST /v1/images/generations`）。
- 代理会从请求中提取 `model`（JSON、`application/x-www-form-urlencoded`、`multipart/form-data`）并在 `allowed_models` 范围内选择目标。
- 代理转发前会移除内部路由参数（`target`）和 Azure 旧版查询参数（`api-version`），避免污染 Azure v1 请求。
- 代理会对核心 JSON 接口执行 Azure 参数白名单过滤，详见 `docs/azure-parameter-whitelist.md`。

## Azure v1 兼容验证
可以用以下方式快速验证某个 endpoint 的模型是否可用（v1 模型检索接口）：

```sh
curl -sS \
  -H "api-key: <azure-api-key>" \
  "https://<resource>.openai.azure.com/openai/v1/models/<model-name>"
```

若返回 `200` 且对象中包含 `id`，说明该模型在该 endpoint 可用。

管理端支持热加载：
```sh
curl -X POST -b "azure_proxy_admin_session=<session-cookie>" \
     http://localhost:8080/admin/config/reload
```

## 日志
默认输出位置为 `logs/access.log` 与 `logs/error.log`。可在配置文件中调整路径或轮转策略，确保满足部署环境的磁盘与合规要求。

## 后台管理系统
后台管理系统采用**独立账号密码**登录，与客户端代理鉴权完全分离。管理员账号存放在 `config/admin_users.json`，密码以 `sha256$<salt>$<hex>` 格式哈希存储。

- **登录入口**：浏览器访问 `http://localhost:8080/login`，输入管理员账号密码完成登录。
- **会话管理**：登录后通过 cookie（`azure_proxy_admin_session`）维持会话，支持滑动过期与登出。
- **管理台**：登录后进入 `/admin`，左侧导航包含：总览、客户端管理、模型费用、消费统计、审计日志。
- **默认账号**：首次部署自带 `admin` / `admin123`，**生产环境请立即更换密码**。

未登录访问 `/admin/*` 会自动跳转到登录页。

## 管理接口
所有管理接口（`/admin/*`）均受 session cookie 鉴权保护，需先通过 `/login` 登录：

| 接口路径              | 方法 | 说明                         |
|-----------------------|------|------------------------------|
| `/login`              | GET/POST | 管理员登录页 / 登录提交    |
| `/logout`             | POST | 登出并销毁 session           |
| `/admin/`             | GET  | 后台管理台入口（左侧导航式） |
| `/admin/api/me`       | GET  | 当前登录用户信息             |
| `/admin/api/overview` | GET  | 总览数据（指标、目标、消费） |
| `/admin/api/audit`    | GET  | 审计日志查询                 |
| `/admin/healthz`      | GET  | 返回目标状态、静默窗口、统计 |
| `/admin/metrics`      | GET  | 聚合请求计数与当前运行指标   |
| `/admin/config/reload`| POST | 从磁盘重新加载配置           |
| `/admin/data/*`       | GET/POST/PUT/DELETE | 客户端、模型费用、消费统计 JSON API |

## 测试
- 运行单元测试：
  ```sh
  make test
  ```
- 带静默切换场景的集成测试：
  ```sh
  ./scripts/run-integration-tests.sh
  # 或 PowerShell
  ./scripts/run-integration-tests.ps1
  ```

## 部署提示
- 参考 `deploy/systemd/azure-proxy.service` 的 systemd 模板进行进程托管。
- 确认运行账号具备读取配置、写入日志的权限。
- 上线前按照 `docs/operations.md` 的检查清单核对环境。
- 若在容器环境部署，请参考 `docs/docker-deploy.md`。

## 常见问题与排查
- 配置或上游错误可在 `logs/error.log` 中查看详情。
- `/admin/healthz` 可用来确认目标是否被静默、故障切换是否生效。
- 若收到 403，检查客户端令牌与 `allowed_targets` 是否匹配。

## 补充文档
- `docs/api-contract.md`：接口契约与错误码说明。
- `docs/internal-training.md`：团队内部培训大纲。
