# Azure 参数白名单（代理侧）

为保证 OpenAI 兼容请求在 Azure v1 下稳定运行，代理会对部分 JSON 接口做顶层参数白名单过滤：

- 仅保留白名单中的字段；
- 白名单外字段会被自动剥离（例如 `prompt_cache_retention`）；
- 当前阶段不做参数映射，只做过滤。

## 1) `POST /v1/responses`

允许字段：

- `background`
- `include`
- `input`
- `instructions`
- `max_output_tokens`
- `max_tool_calls`
- `metadata`
- `model`
- `parallel_tool_calls`
- `previous_response_id`
- `prompt`
- `prompt_cache_key`
- `reasoning`
- `store`
- `stream`
- `temperature`
- `text`
- `tool_choice`
- `tools`
- `top_logprobs`
- `top_p`
- `truncation`
- `user`

## 2) `POST /v1/chat/completions`

允许字段：

- `audio`
- `data_sources`
- `frequency_penalty`
- `function_call`
- `functions`
- `logit_bias`
- `logprobs`
- `max_completion_tokens`
- `max_tokens`
- `messages`
- `metadata`
- `modalities`
- `model`
- `n`
- `parallel_tool_calls`
- `prediction`
- `presence_penalty`
- `prompt_cache_key`
- `reasoning_effort`
- `response_format`
- `seed`
- `stop`
- `store`
- `stream`
- `stream_options`
- `temperature`
- `tool_choice`
- `tools`
- `top_logprobs`
- `top_p`
- `user`
- `user_security_context`

## 3) `POST /v1/embeddings`

允许字段：

- `dimensions`
- `encoding_format`
- `input`
- `model`
- `user`

## 4) 说明

- 白名单匹配基于请求路径后缀（例如 `/v1/responses`、`/openai/v1/responses` 都会命中）。
- 仅处理 JSON 对象请求体的顶层字段；非 JSON 或非对象结构保持透传。
- 未命中白名单的接口（例如音频 multipart 类接口）当前保持透传。
