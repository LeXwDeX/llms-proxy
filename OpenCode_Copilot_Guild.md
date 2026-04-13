# OpenCode Provider 配置说明

> 配置文件路径：`~/.config/opencode/opencode.json`
> 代理地址：`http://192.168.33.110:8000`
> 更新日期：2026-04-13

---

## 架构概览

```
opencode
  ├─ Copilot-Claude         ── 代理 → Copilot 上游（Claude 模型，thinking 模式）
  ├─ Copilot-OpenAI         ── 代理 → Copilot 上游（GPT reasoning 模型）
  ├─ Copilot-OpenAI-Free    ── 代理 → Copilot 上游（GPT 免费模型）
  ├─ API-OpenAI             ── 代理 → Azure/OpenAI 上游
  └─ API-Claude             ── 代理 → 网宿 Claude 上游（Anthropic 原生协议）
```

所有通道统一走 `192.168.33.110:8000` 代理，由代理按模型名前缀和 `endpoint_type` 路由到不同上游。

## 分组与 Provider 对应关系

| 分组名 | Provider ID | SDK | 说明 |
|--------|-------------|-----|------|
| **Copilot-Claude** | `copilot-claude` | `@ai-sdk/openai-compatible` | Copilot Claude 模型，支持 thinking variants |
| **Copilot-OpenAI** | `copilot-openai` | `@ai-sdk/openai` | Copilot GPT reasoning 模型，支持 reasoningEffort |
| **Copilot-OpenAI-Free** | `copilot-openai-free` | `@ai-sdk/openai-compatible` | Copilot GPT 免费模型 |
| **API-OpenAI** | `proxy-openai` | `@ai-sdk/openai` | 直连 Azure/OpenAI 上游 |
| **API-Claude** | `proxy-anthropic` | `@ai-sdk/anthropic` | 直连网宿 Claude 上游（原生协议） |

> `copilot-openai` 和 `copilot-openai-free` 使用不同 SDK，
> 原因是 GPT reasoning 模型需要 `@ai-sdk/openai`（支持 `/responses` 端点和 `reasoningEffort`），
> 免费模型只需 `@ai-sdk/openai-compatible`（仅走 `/chat/completions`）。

---

## Provider 详细配置

### 1. Copilot-Claude

通过代理转发到 Copilot 上游的 Claude 模型。支持 thinking 模式（代理层完整透传 `thinking` 参数和响应）。

```json
"copilot-claude": {
  "npm": "@ai-sdk/openai-compatible",
  "name": "Copilot-Claude",
  "options": {
    "baseURL": "http://192.168.33.110:8000/v1",
    "apiKey": "sk-st0868"
  },
  "models": {
    "Copilot claude-sonnet-4.6": {
      "name": "Copilot claude-sonnet-4.6",
      "variants": {
        "low":    { "thinking": { "type": "enabled", "budgetTokens": 4000  } },
        "medium": { "thinking": { "type": "enabled", "budgetTokens": 8000  } },
        "high":   { "thinking": { "type": "enabled", "budgetTokens": 16000 } },
        "max":    { "thinking": { "type": "enabled", "budgetTokens": 32000 } }
      }
    },
    "Copilot claude-opus-4.6": {
      "name": "Copilot claude-opus-4.6",
      "variants": {
        "low":    { "thinking": { "type": "enabled", "budgetTokens": 4000  } },
        "medium": { "thinking": { "type": "enabled", "budgetTokens": 8000  } },
        "high":   { "thinking": { "type": "enabled", "budgetTokens": 16000 } },
        "max":    { "thinking": { "type": "enabled", "budgetTokens": 32000 } }
      }
    }
  }
}
```

**关键点**：
- 模型名必须带 `Copilot ` 前缀（大写 C + 空格），代理根据此前缀路由到 Copilot 处理链
- `thinking` variants 通过 Copilot 上游的 OpenAI 兼容格式透传，代理层不做过滤
- `apiKey` 是代理的客户端令牌，不是 Copilot 的 OAuth token

---

### 2. Copilot-OpenAI（Reasoning 模型）

通过代理转发到 Copilot 上游的 GPT reasoning 模型。使用 OpenAI 原生 SDK，支持 `/responses` 端点和 `reasoningEffort`。

```json
"copilot-openai": {
  "npm": "@ai-sdk/openai",
  "name": "Copilot-OpenAI",
  "options": {
    "baseURL": "http://192.168.33.110:8000/v1",
    "apiKey": "sk-st0868"
  },
  "models": {
    "Copilot gpt-5.3-codex": {
      "name": "Copilot gpt-5.3-codex",
      "variants": {
        "low":    { "reasoningEffort": "low"  },
        "medium": { "reasoningEffort": "medium" },
        "high":   { "reasoningEffort": "high" }
      }
    },
    "Copilot gpt-5.4": {
      "name": "Copilot gpt-5.4",
      "variants": {
        "low":    { "reasoningEffort": "low"   },
        "medium": { "reasoningEffort": "medium" },
        "high":   { "reasoningEffort": "high"  },
        "xhigh":  { "reasoningEffort": "xhigh" }
      }
    }
  }
}
```

**关键点**：
- GPT-5.3-codex **必须**用此 provider（需要 `/responses` 端点，`/chat/completions` 返回 `unsupported_api_for_model`）
- GPT-5.4 **建议**用此 provider（可调 `reasoningEffort`）

---

### 3. Copilot-OpenAI-Free

通过代理转发到 Copilot 上游的免费 GPT 模型。只走 `/chat/completions`，无需 reasoning 能力。

```json
"copilot-openai-free": {
  "npm": "@ai-sdk/openai-compatible",
  "name": "Copilot-OpenAI-Free",
  "options": {
    "baseURL": "http://192.168.33.110:8000/v1",
    "apiKey": "sk-st0868"
  },
  "models": {
    "Copilot gpt-4o":      { "name": "Copilot gpt-4o" },
    "Copilot gpt-4.1":     { "name": "Copilot gpt-4.1" },
    "Copilot gpt-4o-mini":  { "name": "Copilot gpt-4o-mini" }
  }
}
```

**关键点**：
- 免费模型（premium request 乘数为 0），不消耗 Copilot 高级额度
- 无 variants（这些模型不支持 reasoning 或 thinking 参数）

---

### 4. API-OpenAI

通过代理转发到 Azure OpenAI 或原生 OpenAI 上游（非 Copilot 通道）。

```json
"proxy-openai": {
  "npm": "@ai-sdk/openai",
  "name": "API-OpenAI",
  "options": {
    "baseURL": "http://192.168.33.110:8000/v1",
    "apiKey": "sk-st0868"
  },
  "models": {
    "gpt-5.3-codex": { "name": "GPT-5.3 Codex",  "variants": { ... } },
    "gpt-5.4-mini":  { "name": "GPT-5.4 Mini",   "variants": { ... } },
    "gpt-5.4-nano":  { "name": "GPT-5.4 Nano",   "variants": { ... } }
  }
}
```

**注意**：模型名不带 `Copilot ` 前缀，代理按 `endpoint_type` 路由到 Azure/OpenAI 目标。

---

### 5. API-Claude

通过代理转发到网宿 Claude 上游，使用 Anthropic 原生协议。

```json
"proxy-anthropic": {
  "npm": "@ai-sdk/anthropic",
  "name": "API-Claude",
  "options": {
    "baseURL": "http://192.168.33.110:8000/v1"
  },
  "models": {
    "claude-opus-4-6": {
      "name": "Claude Opus 4.6 (Native)",
      "limit": { "context": 1048576, "output": 32000 },
      "variants": {
        "low":    { "thinking": { "type": "enabled", "budgetTokens": 4000  } },
        "medium": { "thinking": { "type": "enabled", "budgetTokens": 8000  } },
        "high":   { "thinking": { "type": "enabled", "budgetTokens": 16000 } },
        "max":    { "thinking": { "type": "enabled", "budgetTokens": 32000 } }
      }
    },
    "claude-sonnet-4-6": {
      "name": "Claude Sonnet 4.6 (Native)",
      "variants": {
        "low":    { "thinking": { "type": "enabled", "budgetTokens": 4000  } },
        "medium": { "thinking": { "type": "enabled", "budgetTokens": 8000  } },
        "high":   { "thinking": { "type": "enabled", "budgetTokens": 16000 } },
        "max":    { "thinking": { "type": "enabled", "budgetTokens": 32000 } }
      }
    }
  }
}
```

**注意**：
- 模型 ID 用连字符 `claude-opus-4-6`（Anthropic SDK 约定），不是点号
- `thinking` variants 通过 Anthropic 原生 SDK 传递
- 无需 `apiKey`（代理侧已配置上游 key）

---

## 模型完整列表

```
copilot-claude/Copilot claude-opus-4.6       # thinking: low/medium/high/max
copilot-claude/Copilot claude-sonnet-4.6     # thinking: low/medium/high/max
copilot-openai/Copilot gpt-5.3-codex        # reasoningEffort: low/medium/high
copilot-openai/Copilot gpt-5.4              # reasoningEffort: low/medium/high/xhigh
copilot-openai-free/Copilot gpt-4.1         # 免费
copilot-openai-free/Copilot gpt-4o          # 免费
copilot-openai-free/Copilot gpt-4o-mini     # 免费
proxy-anthropic/claude-opus-4-6             # thinking: low/medium/high/max
proxy-anthropic/claude-sonnet-4-6           # thinking: low/medium/high/max
proxy-openai/gpt-5.3-codex                 # reasoningEffort: low/medium/high
proxy-openai/gpt-5.4-mini                  # reasoningEffort: low/medium/high
proxy-openai/gpt-5.4-nano                  # reasoningEffort: low/medium/high
```

---

## Copilot 当前不支持的模型

以下模型上游返回 `model_not_supported`，可能需要更高级别的 Copilot 订阅（Pro+）：

claude-haiku-4.5, claude-sonnet-4, claude-sonnet-4.5, gemini-2.5-pro, gemini-3-flash, gemini-3.1-pro, gpt-5-mini, gpt-5.4-mini

---

## 避免重复的原则

1. **同一模型 ID 不要出现在多个 provider 中**（opencode 模型选择列表会显示重复项）
2. Claude 模型只放 `copilot-claude`，GPT reasoning 放 `copilot-openai`，GPT 免费放 `copilot-openai-free`
3. `proxy-openai` / `proxy-anthropic` 的模型走独立 API 上游，与 Copilot 是不同通道，模型名无 `Copilot ` 前缀，不会冲突

---

## 新增模型的步骤

1. 确认模型在 Copilot 上游可用：
   ```bash
   curl -X POST http://192.168.33.110:8000/v1/chat/completions \
     -H "Authorization: Bearer sk-st0868" \
     -H "Content-Type: application/json" \
     -d '{"model":"Copilot <模型名>","messages":[{"role":"user","content":"hi"}],"max_tokens":5}'
   ```
2. 如果返回 `unsupported_api_for_model`，改用 `/responses` 端点测试
3. 根据结果决定放入哪个 provider：
   - Claude 模型 → `copilot-claude`
   - 需要 `/responses` 或 `reasoningEffort` → `copilot-openai`
   - 免费基础模型 → `copilot-openai-free`
4. 在 `enabled_providers` 中确认对应 provider 已启用
