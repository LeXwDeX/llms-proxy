# Claude 端点与请求/回应结构

本文档整理 Anthropic / Claude 的官方接口、请求结构与回应结构，并补充本仓库当前的适配边界。

## 官方端点
- Base URL：`https://api.anthropic.com`
- `POST /v1/messages`：主消息接口。
- `POST /v1/messages/count_tokens`：输入 token 计数。
- `GET /v1/models`：模型列表。

## 鉴权与请求头
- `x-api-key: <ANTHROPIC_API_KEY>`
- `anthropic-version: 2023-06-01`
- `Content-Type: application/json`

## 1) Messages API

### 请求结构
`POST /v1/messages` 的核心字段如下：

| 字段 | 说明 |
|---|---|
| `model` | 模型 ID，必填。|
| `max_tokens` | 最大生成 token，必填。|
| `messages` | 输入消息数组，必填。|
| `system` | 顶层系统提示。|
| `tools` | 工具定义数组。|
| `tool_choice` | 工具选择策略。|
| `temperature` | 采样温度。|
| `top_p` | nucleus sampling。|
| `top_k` | top-k 采样。|
| `stop_sequences` | 自定义停止序列。|
| `thinking` | 扩展思考配置。|
| `metadata` | 元数据。|
| `output_config` | 输出格式配置。|
| `service_tier` | 服务等级。|
| `stream` | 是否流式。|
| `cache_control` | 顶层缓存控制。|
| `container` | 容器复用标识。|
| `inference_geo` | 推理区域。|

### 消息输入结构
`messages[]` 中每项通常为：

```json
{ "role": "user", "content": "Hello, Claude" }
```

也可写成块数组：

```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "Hello, Claude"}
  ]
}
```

常见输入块类型：
- `text`
- `image`
- `document`
- `tool_result`
- `tool_use`（用于工具交互场景）

### 典型请求示例
```json
{
  "model": "claude-opus-4-6",
  "max_tokens": 1024,
  "system": "You are a helpful assistant.",
  "messages": [
    {"role": "user", "content": "Hello, Claude"}
  ]
}
```

### 回应结构
`Message` 对象核心字段如下：

| 字段 | 说明 |
|---|---|
| `id` | 唯一消息 ID。|
| `type` | 固定为 `message`。|
| `role` | 固定为 `assistant`。|
| `content` | 内容块数组。|
| `model` | 实际使用的模型。|
| `stop_reason` | 停止原因。|
| `stop_sequence` | 命中的停止序列。|
| `usage` | token 使用统计。|
| `container` | 容器信息（如有）。|

`stop_reason` 常见值：
- `end_turn`
- `max_tokens`
- `stop_sequence`
- `tool_use`
- `pause_turn`
- `refusal`

### 常见输出块
- `text`：文本输出。
- `thinking`：扩展思考内容。
- `tool_use`：模型发起工具调用。
- `redacted_thinking`：被脱敏的思考块。

### 典型回应示例
```json
{
  "id": "msg_123",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-4-6",
  "stop_reason": "end_turn",
  "content": [
    {"type": "text", "text": "Hi!"}
  ],
  "usage": {"input_tokens": 10, "output_tokens": 2}
}
```

## 2) Token 计数

### 请求结构
- `POST /v1/messages/count_tokens`
- 输入结构与 `messages` 接口一致：`model`、`messages`、`system`、`tools` 等。

### 回应结构
```json
{ "input_tokens": 123 }
```

## 3) Models
- `GET /v1/models`
- 返回模型列表对象；模型 ID 应使用官方名称。

## 约束与注意事项
- `messages` 的组织方式与 OpenAI 不同，不要直接把 OpenAI 请求体原样发送给 Claude。
- 系统提示请使用顶层 `system`，不要使用 `system` role。
- 若需要图片、文档或工具调用，请遵循 Anthropic content block 规范。

## 本仓库使用提示
- 本仓库当前实现的是 Azure OpenAI 代理，不直接提供 Claude 网关。
- 如果后续接入 Claude，应单独实现路由、鉴权与兼容层，并按 Anthropic 官方协议对接。
