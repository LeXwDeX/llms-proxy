// copilot_initiator.go — Decide the X-Initiator header value for Copilot
// upstream requests, so premium-request billing reflects the true caller.
//
// GitHub Copilot bills:
//   - X-Initiator: user  → 1 premium × model multiplier
//   - X-Initiator: agent → autonomous turn, NOT charged
//   - missing / invalid → defaults to user (full charge)
//
// Agentic clients (Claude Code, etc.) do not send the header, so naive proxying
// gets charged for every internal tool turn. inferInitiator inspects the
// request body to label tool / mid-turn / assistant continuations as "agent",
// and only true user prompts as "user".
package proxy

import (
	"encoding/json"
	"strings"
)

// inferInitiator decides the X-Initiator header value for a Copilot upstream
// request.
//
// Priority:
//  1. If downstreamValue is non-empty and (case-insensitively) "user" or
//     "agent", return it normalized — the client knows the rules, respect it.
//     Any other non-empty value is ignored and falls through to body inference.
//  2. Parse body and infer from the last message (Chat Completions / Anthropic
//     Messages format), or the last `input` item for OpenAI Responses API
//     requests (POST /v1/responses, top-level `input` instead of `messages`):
//     - Role == "tool"           (OpenAI tool-result message) → "agent"
//     - Role == "assistant"      (mid-turn continuation)      → "agent"
//     - Role == "user":
//     - Content is a string (OpenAI simple format)        → "user"
//     - Content is an array (Anthropic / Responses blocks):
//     - any block.type == "tool_result"               → "agent"
//     - otherwise (text / input_text / input_image…)  → "user"
//     - Responses API `input` field:
//     - string                                            → "user"
//     - empty array                                       → "agent"
//     - array, last item has role → same role rules as above
//     - array, last item is typed object without role
//     (function_call_output / function_call / reasoning /
//     *_call / *_call_output etc.)                    → "agent"
//  3. Fallback when body is empty / unparseable / messages empty / any other
//     unexpected shape → "agent" (conservative: save the customer money;
//     Copilot ToS punishes forged "user", not under-claiming).
//
// The function never returns an empty string.
func inferInitiator(body []byte, downstreamValue string) string {
	// 1. Respect a valid downstream value verbatim (normalized lowercase).
	if v := strings.ToLower(strings.TrimSpace(downstreamValue)); v == "user" || v == "agent" {
		return v
	}

	// 2. Parse body. Anything wrong → fallback agent.
	if len(body) == 0 {
		return "agent"
	}

	var envelope struct {
		Messages []json.RawMessage `json:"messages"`
		Input    json.RawMessage   `json:"input"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "agent"
	}

	// Prefer messages (Chat Completions / Anthropic Messages format).
	if len(envelope.Messages) > 0 {
		return classifyLastMessage(envelope.Messages[len(envelope.Messages)-1])
	}

	// Otherwise try OpenAI Responses API `input` field.
	if len(envelope.Input) > 0 {
		return classifyResponsesInput(envelope.Input)
	}

	return "agent"
}

// classifyLastMessage applies the messages-array rules to a single message.
func classifyLastMessage(raw json.RawMessage) string {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return "agent"
	}

	switch msg.Role {
	case "tool":
		// OpenAI tool-result message — purely an agent turn.
		return "agent"
	case "assistant":
		// Mid-turn assistant continuation — agent.
		return "agent"
	case "user":
		return classifyUserContent(msg.Content)
	default:
		return "agent"
	}
}

// classifyResponsesInput handles the top-level `input` field of POST /v1/responses.
// The input may be a plain string (shorthand single user turn) or an array of
// typed items (EasyInputMessage with role, or function_call / function_call_output
// / reasoning / mcp_call / *_call_output etc. without role).
func classifyResponsesInput(input json.RawMessage) string {
	trimmed := bytesTrimLeftSpace(input)
	if len(trimmed) == 0 {
		return "agent"
	}
	switch trimmed[0] {
	case '"':
		// Shorthand string input → single-turn user prompt.
		return "user"
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(input, &items); err != nil {
			return "agent"
		}
		if len(items) == 0 {
			return "agent"
		}
		last := items[len(items)-1]
		var item struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(last, &item); err != nil {
			return "agent"
		}
		if item.Role == "" {
			// Typed object without role: function_call_output, function_call,
			// reasoning, mcp_call, *_call_output, etc. — all agent-side.
			return "agent"
		}
		switch item.Role {
		case "user":
			return classifyUserContent(item.Content)
		default:
			// assistant / tool / system / unknown — agent.
			return "agent"
		}
	default:
		return "agent"
	}
}

// classifyUserContent decides whether a user-role message's content represents
// a true user prompt ("user") or a tool-result handed back by an agent loop
// ("agent"). The Anthropic Messages API delivers tool results as a content
// block of type "tool_result" inside a message with role="user".
func classifyUserContent(content json.RawMessage) string {
	trimmed := bytesTrimLeftSpace(content)
	if len(trimmed) == 0 {
		// Empty content — treat as agent to be safe.
		return "agent"
	}
	switch trimmed[0] {
	case '"':
		// OpenAI simple string content → real user prompt.
		return "user"
	case '[':
		// Anthropic content blocks array → inspect each block's type.
		var blocks []struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(content, &blocks); err != nil {
			return "agent"
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return "agent"
			}
		}
		return "user"
	default:
		// Unknown shape (null, number, object) → conservative agent.
		return "agent"
	}
}

// bytesTrimLeftSpace returns b with leading ASCII / JSON whitespace stripped.
func bytesTrimLeftSpace(b []byte) []byte {
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b[i:]
		}
	}
	return nil
}
