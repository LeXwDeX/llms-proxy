# CopilotKit Integration Guide

This guide explains how to integrate [CopilotKit](https://github.com/CopilotKit/CopilotKit) with llms-proxy.

## Overview

CopilotKit is an open-source framework for building AI copilot UIs in React/Next.js applications. It supports multiple LLM providers through its service adapter system.

llms-proxy can serve as a backend for CopilotKit's `OpenAIAdapter`, routing requests to any configured upstream target (OpenAI, Azure OpenAI, Claude, Gemini, etc.).

## Supported Adapters

### ✅ `OpenAIAdapter` (Recommended)

Use this adapter. It calls standard OpenAI-compatible endpoints that llms-proxy already supports.

| Endpoint | Status | Usage |
|----------|--------|-------|
| `/v1/chat/completions` | ✅ Supported | Core chat + tool calling + streaming |
| `/v1/embeddings` | ✅ Supported | Vector embeddings (text-embedding-3-*) |

**Configuration:**

```typescript
import { CopilotRuntime, OpenAIAdapter } from "@copilotkit/runtime";
import OpenAI from "openai";

const openai = new OpenAI({
  baseURL: "http://192.168.33.110:8000",  // llms-proxy endpoint
  apiKey: "sk-your-client-token",         // llms-proxy client access token
});

const runtime = new CopilotRuntime({
  serviceAdapter: new OpenAIAdapter({ 
    openai, 
    model: "gpt-4o"  // or any model configured in llms-proxy
  }),
});
```

**Why this works:**
- CopilotKit's `OpenAIAdapter` calls `/v1/chat/completions` (standard OpenAI format)
- llms-proxy already handles OpenAI-compatible requests, SSE streaming, tool calls
- Zero code changes required in llms-proxy

### ❌ `OpenAIAssistantAdapter` (Not Supported)

Do **not** use this adapter. It requires OpenAI's Assistants API (`/v1/assistants`, `/v1/threads`, etc.), which llms-proxy does not support.

**Why not supported:**
1. **Stateful protocol** — Assistants API maintains conversation state on OpenAI's servers (threads, runs, vector stores). llms-proxy is a stateless HTTP proxy.
2. **OpenAI-specific** — No equivalent protocol exists for other providers (Claude, Gemini, etc.), defeating llms-proxy's multi-provider routing purpose.
3. **Architectural mismatch** — `runs.create` triggers long-running server-side loops on OpenAI. Proxy cannot meaningfully cache, retry, or failover these operations.

**If you need Assistants API functionality:**
- Connect directly to OpenAI (bypass llms-proxy)
- Consider implementing agent logic client-side using `OpenAIAdapter` + tool calling

## Verified Endpoints

llms-proxy has been verified to support these endpoints used by CopilotKit:

### `/v1/chat/completions`

Standard OpenAI chat completions with full feature support:

- ✅ Non-streaming responses
- ✅ SSE streaming (`stream: true`)
- ✅ Tool/function calling (`tools`, `tool_calls`)
- ✅ Token usage tracking (`usage` object in response)
- ✅ Model enforcement (`allowed_models` in target config)
- ✅ Failover across multiple targets

**Test with curl:**

```bash
curl -X POST http://192.168.33.110:8000/v1/chat/completions \
  -H "Authorization: Bearer sk-your-client-token" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'
```

### `/v1/embeddings`

Text embedding generation:

- ✅ `text-embedding-3-small`, `text-embedding-3-large`
- ✅ Batch processing (`input` as array)
- ✅ Custom dimensions (`dimensions` parameter)

**Test with curl:**

```bash
curl -X POST http://192.168.33.110:8000/v1/embeddings \
  -H "Authorization: Bearer sk-your-client-token" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "text-embedding-3-small",
    "input": "Hello world"
  }'
```

## Client Configuration

Ensure your llms-proxy client has appropriate permissions:

1. **Create/update client** in admin UI (`/admin/data/clients`):
   - Generate an access token (e.g., `sk-copilot-frontend`)
   - Set `allowed_targets` to appropriate upstream targets (or `["*"]` for all)
   - Optionally configure `allowed_models` to restrict which models this client can use

2. **Verify upstream targets** have the required models in their `allowed_models` list (if configured)

3. **Test connectivity:**
   ```bash
   curl -X GET http://192.168.33.110:8000/v1/models \
     -H "Authorization: Bearer sk-copilot-frontend"
   ```

## Known Limitations

### Streaming Error Handling

When using `stream: true`, llms-proxy forwards SSE events from upstream. If upstream returns an error mid-stream, llms-proxy propagates it to CopilotKit. CopilotKit's error handling varies by error type:

- **Rate limits (429)** — CopilotKit may retry automatically
- **Content filter violations** — Typically returns as a message with `finish_reason: "content_filter"`
- **Network failures** — Causes connection reset; CopilotKit surfaces this to the user

### Tool Calling

CopilotKit uses OpenAI's tool calling protocol. Ensure:

1. Your chosen model supports function calling (check `allowed_models` in target config)
2. The target's `endpoint_type` is OpenAI-compatible (`openai`, `azure_openai`, `wangsu_openai`)
3. Claude/Gemini targets require `tool_choice` mapping (handled by llms-proxy automatically)

### Model Selection

CopilotKit passes `model` in the request body. llms-proxy routes based on:

1. `model` value matches a target's `allowed_models`
2. Client has access to that target (via `allowed_targets`)
3. If multiple targets support the model, routing follows affinity + round-robin

**Best practice:** Explicitly specify `model` in `OpenAIAdapter` config to avoid ambiguous routing.

## Troubleshooting

### "Model not found" error

**Cause:** The model isn't in any target's `allowed_models` list, or the client doesn't have access to that target.

**Fix:**
1. Check target config: `GET /admin/data/targets/{name}`
2. Ensure `allowed_models` includes the model you're requesting
3. Verify client's `allowed_targets` permits access to that target

### Streaming hangs or incomplete response

**Cause:** Upstream timeout or network issue mid-stream.

**Fix:**
1. Check `logs/error.log` for upstream errors
2. Verify `config.json` → `server.request_timeout_seconds` (default 300s)
3. For long conversations, consider increasing timeout

### Tool calls not executing

**Cause:** Model doesn't support function calling, or upstream doesn't honor `tool_calls` in response.

**Fix:**
1. Verify model supports function calling (e.g., `gpt-4o`, `gpt-4-turbo`, `claude-3-5-sonnet`)
2. Check that `endpoint_type` is OpenAI-compatible
3. Test directly with curl to isolate whether issue is in CopilotKit or llms-proxy

### 401 Unauthorized

**Cause:** Invalid or missing `apiKey` in OpenAI client config.

**Fix:**
1. Verify `apiKey` matches a client token in llms-proxy (`GET /admin/data/clients`)
2. Ensure token doesn't have leading/trailing whitespace
3. Check client is active (not disabled)

## Example: Full Next.js Integration

```tsx
// app/api/copilotkit/route.ts
import { CopilotRuntime, OpenAIAdapter } from "@copilotkit/runtime";
import OpenAI from "openai";

const openai = new OpenAI({
  baseURL: process.env.LLMS_PROXY_URL || "http://192.168.33.110:8000",
  apiKey: process.env.LLMS_PROXY_TOKEN || "sk-your-client-token",
});

const runtime = new CopilotRuntime({
  serviceAdapter: new OpenAIAdapter({ 
    openai, 
    model: process.env.LLMS_MODEL || "gpt-4o"
  }),
});

export async function POST(req: Request) {
  return runtime.streamCallback();
}

// app/page.tsx
import { CopilotKit } from "@copilotkit/react-core";
import { CopilotChat } from "@copilotkit/react-ui";

export default function Home() {
  return (
    <CopilotKit runtimeUrl="/api/copilotkit">
      <CopilotChat 
        instructions="You are a helpful assistant."
        labels={{
          title: "AI Assistant",
          initial: "How can I help you today?"
        }}
      />
    </CopilotKit>
  );
}
```

## Summary

| Aspect | Status |
|--------|--------|
| **Adapter** | Use `OpenAIAdapter` only |
| **Endpoints** | `/v1/chat/completions` (chat), `/v1/embeddings` (embeddings) |
| **llms-proxy changes** | None required |
| **Configuration** | Set `baseURL` + `apiKey` in OpenAI client |
| **Streaming** | Supported (SSE) |
| **Tool calling** | Supported (OpenAI function calling protocol) |
| **Assistants API** | Not supported (use `OpenAIAdapter` instead) |

## Further Reading

- [CopilotKit Documentation](https://docs.copilotkit.ai)
- [OpenAI SDK Reference](https://github.com/openai/openai-node)
- [llms-proxy API Contract](./api-contract.md)
