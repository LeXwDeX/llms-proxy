// sse_aggregate.go — 将 SSE streaming 响应聚合为标准 non-streaming ChatCompletion JSON。
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// extractStreamField extracts the "stream" boolean field from a JSON request body.
// Returns (streamValue, found). If the field is absent or not a bool, found is false.
func extractStreamField(body []byte) (streamValue bool, found bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return false, false
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false, false
	}

	streamRaw, exists := raw["stream"]
	if !exists {
		return false, false
	}

	var b bool
	if err := json.Unmarshal(streamRaw, &b); err != nil {
		// "stream" exists but is not a bool (e.g. string or object).
		return false, false
	}
	return b, true
}

// shouldAggregateSSE decides whether to aggregate an SSE response into a
// standard JSON ChatCompletion response.
//
// Aggregation triggers when ALL of:
//  1. target.SSEAutoAggregate is true
//  2. The client did NOT request streaming (stream=true absent)
//  3. The upstream responded with text/event-stream Content-Type
func shouldAggregateSSE(target *Target, requestBody []byte, respContentType string) bool {
	if target == nil || !target.SSEAutoAggregate {
		return false
	}

	// If the client explicitly asked for streaming, pass through as-is.
	if streamVal, found := extractStreamField(requestBody); found && streamVal {
		return false
	}

	// Check whether the upstream response is SSE.
	ct := strings.ToLower(strings.TrimSpace(respContentType))
	return strings.Contains(ct, "text/event-stream")
}

// sseChunk is a minimal representation of an SSE ChatCompletionChunk.
type sseChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role,omitempty"`
			Content string `json:"content,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *sseUsage `json:"usage,omitempty"`
}

type sseUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// aggregateSSEResponse reads a complete SSE body and assembles it into a
// standard ChatCompletion JSON response. It returns the JSON bytes, the new
// Content-Type string, and any error.
func aggregateSSEResponse(body []byte) ([]byte, string, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, "", fmt.Errorf("sse_aggregate: empty body")
	}

	var (
		id           string
		model        string
		created      int64
		role         string
		contentParts []string
		finishReason string
		usageInfo    *sseUsage
		chunkCount   int
	)

	scanner := bufio.NewScanner(bytes.NewReader(body))
	buf := make([]byte, 0, 4096)
	scanner.Buffer(buf, 2*1024*1024) // up to 2MB per line

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		chunkCount++

		// Extract metadata from the first valid chunk.
		if id == "" && chunk.ID != "" {
			id = chunk.ID
		}
		if model == "" && chunk.Model != "" {
			model = chunk.Model
		}
		if created == 0 && chunk.Created != 0 {
			created = chunk.Created
		}

		// Accumulate content from choices[0].delta.
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			if delta.Role != "" && role == "" {
				role = delta.Role
			}
			if delta.Content != "" {
				contentParts = append(contentParts, delta.Content)
			}
			if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
				finishReason = *chunk.Choices[0].FinishReason
			}
		}

		// Usage is typically in the last (or second-to-last) chunk.
		if chunk.Usage != nil {
			usageInfo = chunk.Usage
		}
	}

	if chunkCount == 0 {
		return nil, "", fmt.Errorf("sse_aggregate: no valid SSE data chunks found")
	}

	if role == "" {
		role = "assistant"
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	// Build the standard ChatCompletion response.
	result := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    role,
					"content": strings.Join(contentParts, ""),
				},
				"finish_reason": finishReason,
			},
		},
	}

	if usageInfo != nil {
		result["usage"] = map[string]any{
			"prompt_tokens":     usageInfo.PromptTokens,
			"completion_tokens": usageInfo.CompletionTokens,
			"total_tokens":      usageInfo.TotalTokens,
		}
	}

	out, err := json.Marshal(result)
	if err != nil {
		return nil, "", fmt.Errorf("sse_aggregate: marshal result: %w", err)
	}

	return out, "application/json", nil
}
