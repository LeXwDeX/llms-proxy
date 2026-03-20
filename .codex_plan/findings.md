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

### 追加任务落地状态
- `OPENAI.MD`：已扩写为包含 Responses / Chat Completions 的请求与回应结构说明。
- `CLAUDE.md`：已扩写为包含 Messages / count_tokens 的请求与回应结构说明。
