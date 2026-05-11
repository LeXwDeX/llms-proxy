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
//  2. Parse body and infer from the last message:
//     - Role == "tool"           (OpenAI tool-result message) → "agent"
//     - Role == "assistant"      (mid-turn continuation)      → "agent"
//     - Role == "user":
//     - Content is a string (OpenAI simple format)        → "user"
//     - Content is an array (Anthropic blocks):
//     - any block.type == "tool_result"               → "agent"
//     - otherwise                                     → "user"
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
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "agent"
	}
	if len(envelope.Messages) == 0 {
		return "agent"
	}

	last := envelope.Messages[len(envelope.Messages)-1]
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(last, &msg); err != nil {
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
