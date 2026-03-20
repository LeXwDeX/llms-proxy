# OpenAI 端点与请求/回应结构

本文档整理 OpenAI 官方接口的端点、请求结构与回应结构，并补充本仓库代理入口的兼容注意事项。

## 官方端点
- Base URL：`https://api.openai.com/v1`
- `POST /v1/responses`：推荐主接口。
- `POST /v1/chat/completions`：兼容旧式对话接口。
- `GET /v1/models`：模型列表。
- `POST /v1/responses/input_tokens`：输入 token 统计。

## 鉴权与请求头
- `Authorization: Bearer <OPENAI_API_KEY>`
- `Content-Type: application/json`
- 流式响应使用 `stream: true`，返回 SSE。

## 1) Responses API

### 请求结构
`POST /v1/responses` 的核心字段如下：

| 字段 | 说明 |
|---|---|
| `model` | 模型 ID，必填。|
| `input` | 输入内容，支持字符串或结构化输入（文本/图片/文件/对话项）。|
| `instructions` | 系统/开发者指令。|
| `max_output_tokens` | 最大输出 token 数。|
| `tools` | 工具定义数组。|
| `tool_choice` | 工具选择策略。|
| `stream` | 是否流式输出。|
| `temperature` / `top_p` | 采样参数。|
| `top_logprobs` | 每步返回的候选 logprob 数量。|
| `reasoning` | 推理模型配置（gpt-5 / o 系列）。|
| `metadata` | 16 对键值元数据。|
| `previous_response_id` | 续接上一轮响应。|
| `conversation` | 会话引用。|
| `prompt` | Prompt 模板引用。|
| `background` | 后台执行。|
| `store` | 是否存储响应。|
| `service_tier` | 服务等级。|
| `safety_identifier` | 安全识别标识。|
| `prompt_cache_key` | 缓存键。|
| `prompt_cache_retention` | 缓存保留策略。|
| `truncation` | 截断策略。|
| `text` | 文本输出配置（纯文本 / JSON schema）。|
| `include` | 额外包含字段（如 web search / logprobs / reasoning encrypted content）。|

### 典型请求示例
```json
{
  "model": "gpt-5.2",
  "instructions": "You are a helpful assistant.",
  "input": "Write a one-sentence bedtime story.",
  "max_output_tokens": 120,
  "stream": false
}
```

### 回应结构
`Response` 对象核心字段如下：

| 字段 | 说明 |
|---|---|
| `id` | 响应唯一 ID。|
| `object` | 固定为 `response`。|
| `created_at` | 创建时间戳（秒）。|
| `status` | `completed` / `failed` / `in_progress` / `cancelled` / `queued` / `incomplete`。|
| `model` | 实际使用的模型。|
| `output` | 输出项数组。|
| `usage` | token 使用统计。|
| `error` | 失败时的错误对象。|
| `incomplete_details` | 不完整原因。|
| `conversation` | 会话信息。|
| `output_text` | SDK 便利属性，聚合所有文本输出。|

### 常见输出项
- `message`：助手消息，内部包含 `output_text`、`output_audio`、`refusal` 等块。
- `reasoning`：推理内容。
- `tool_call` / `function_call` / `mcp` / `web_search`：工具调用项。
- `file_search` / `computer_use` / `code_interpreter`：内置工具项。

### 典型回应示例
```json
{
  "id": "resp_123",
  "object": "response",
  "created_at": 1730000000,
  "status": "completed",
  "model": "gpt-5.2",
  "output": [
    {
      "type": "message",
      "content": [
        {"type": "output_text", "text": "Once upon a time..."}
      ]
    }
  ],
  "usage": {"input_tokens": 12, "output_tokens": 18, "total_tokens": 30}
}
```

### 流式说明
- 使用 `stream: true` 时，返回 SSE 事件流。
- 常见事件包括创建、内容块增量、完成、错误等。

### token 统计
- `POST /v1/responses/input_tokens` 用于统计输入 token。

## 2) Chat Completions API

### 请求结构
`POST /v1/chat/completions` 的核心字段如下：

| 字段 | 说明 |
|---|---|
| `messages` | 消息数组，必填。|
| `model` | 模型 ID，必填。|
| `max_completion_tokens` | 最大输出 token 数。|
| `max_tokens` | 旧参数，已弃用，部分模型不兼容。|
| `temperature` / `top_p` | 采样参数。|
| `presence_penalty` / `frequency_penalty` | 惩罚参数。|
| `logprobs` / `top_logprobs` | 结果 token 概率。|
| `n` | 候选数。|
| `stop` | 停止序列。|
| `seed` | 尽力复现采样。|
| `tools` | 函数/自定义工具。|
| `tool_choice` | 工具选择。|
| `response_format` | JSON mode / JSON schema / text。|
| `stream` | 是否流式。|
| `modalities` | 输出模态（text/audio）。|
| `audio` | 音频输出参数。|
| `metadata` | 元数据。|
| `service_tier` | 服务等级。|
| `store` | 是否存储。|
| `user` / `safety_identifier` / `prompt_cache_key` | 用户与缓存相关标识。|
| `reasoning_effort` | 推理强度。|
| `verbosity` | 输出冗长度。|
| `web_search_options` | Web Search 工具参数。|

### 典型请求示例
```json
{
  "model": "gpt-5.2",
  "messages": [
    {"role": "user", "content": "Hello"}
  ],
  "temperature": 0.7,
  "stream": false
}
```

### 回应结构
`ChatCompletion` 对象核心字段如下：

| 字段 | 说明 |
|---|---|
| `id` | 完成结果唯一 ID。|
| `object` | 固定为 `chat.completion`。|
| `created` | 创建时间戳（秒）。|
| `model` | 实际模型。|
| `choices` | 候选数组。|
| `usage` | token 使用统计。|
| `system_fingerprint` | 后端配置指纹。|
| `service_tier` | 实际服务等级。|

`choices[]` 中每项通常包含：
- `index`
- `message`
- `finish_reason`：`stop` / `length` / `tool_calls` / `content_filter` / `function_call`
- `logprobs`

### 典型回应示例
```json
{
  "id": "chatcmpl_123",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "gpt-5.2",
  "choices": [
    {
      "index": 0,
      "finish_reason": "stop",
      "message": {"role": "assistant", "content": "Hi!"}
    }
  ],
  "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}
}
```

## SDK 约定
- Python SDK 默认读取 `OPENAI_API_KEY`。
- 可通过 `base_url` 覆盖 API 根地址；自建代理保留 `/v1` 前缀即可。
- 如果上游是 Azure OpenAI，请改用 `AzureOpenAI`，并配置 `azure_endpoint` 与 `api_version`。

## 本仓库使用提示
- 本仓库对外提供 OpenAI 风格兼容入口，客户端可继续使用上述请求结构。
- 通过本仓库代理 Azure OpenAI 时，请勿手工附带 `target`、`api-version`、`api_version`、`api-key` 等内部/旧参数。
