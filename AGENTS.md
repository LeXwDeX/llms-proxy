# 项目总览

本项目是一个 **Azure OpenAI 代理服务**：对外提供 OpenAI 风格的 HTTP 入口，对内统一转发到多个 Azure OpenAI 终端，帮助内部客户端在一个入口下完成鉴权、路由、日志、故障切换与运维管理。

## 业务目标
- 统一入口：屏蔽多个 Azure 终端差异，对外保持 OpenAI 兼容调用方式。
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
  - `azure_targets`：Azure 终端、路径前缀、API Key、允许模型；
  - `clients`：客户端访问令牌与允许的目标；
  - `logging`：日志级别与日志文件路径。

### 3. 认证层
- `internal/auth` 负责客户端鉴权与授权。
- 支持 `Authorization: Bearer <access-key>`、`api-key` header，以及 `?api-key=` 查询参数。
- `Store` 按配置构建运行时凭据映射；`Principal` 保存客户端名称、是否放通全部目标、允许目标列表。

### 4. 代理层
- `internal/proxy/service.go` 是核心转发逻辑。
- 主要职责：
  - 根据客户端权限与请求目标进行路由；
  - 按 `allowed_models` 做模型约束；
  - 对 Azure v1 不兼容字段做白名单过滤；
  - 转发请求、回写响应、保留流式输出；
  - 记录目标健康状态、失败静默与运行时指标。

### 5. 管理层
- `internal/admin/handler.go` 提供管理接口：
  - `GET /admin/healthz`
  - `GET /admin/metrics`
  - `POST /admin/config/reload`
- 管理接口用于健康检查、指标查看和配置热加载。

### 6. 基础设施层
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
5. 代理层根据目标/模型/权限选择 Azure 后端，必要时执行重试或 failover。
6. 结果透传给客户端，并在响应头中标记 `X-Azure-Target`。

## 关键业务规则
- 客户端令牌与可访问目标绑定；`allowed_targets` 为空表示允许访问全部目标。
- 显式目标可通过 `X-Proxy-Target` 或 `target` 查询参数指定。
- 若目标配置了 `allowed_models`，请求必须携带可识别的 `model`，且模型必须命中白名单。
- 代理会剥离内部/旧版参数：`target`、`api-version`、`api_version`、`api-key`。
- 对部分 JSON 接口执行顶层字段白名单过滤，确保与 Azure v1 兼容。
- 某个目标连续失败后会进入静默窗口，优先切换到其他可用目标。

## 目录说明
- `cmd/proxy/`：应用入口。
- `config/`：示例配置。
- `internal/`：核心实现。
- `docs/`：接口契约、运维手册、参数白名单、发布说明。
- `deploy/`：部署模板（如 systemd）。
- `scripts/`：测试与辅助脚本。
- `test/`：集成测试。
- `logs/`：默认日志目录。

## 维护建议
- 修改接口行为时，优先同步 `docs/api-contract.md`。
- 修改路由/鉴权/转发行为时，优先检查 `internal/auth`、`internal/proxy`、`internal/admin`。
- 修改日志或运维行为时，检查 `internal/logging` 与 `docs/operations.md`。
- 避免在文档中写入真实密钥、令牌或环境专有敏感信息。

## 适用范围
本文件用于帮助后续维护者和自动化代理快速理解项目结构与业务边界；如实现发生变化，应同步更新本文件与对应设计文档。
