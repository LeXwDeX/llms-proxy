// usage.go — 用量 token 提取（JSON/SSE），兼容 OpenAI/Claude/Gemini 格式。
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

type usageTokens struct {
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
}

func extractUsageTokens(contentType string, body []byte) (usageTokens, string, bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return usageTokens{}, "", false
	}

	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(contentType, "text/event-stream") || bytes.HasPrefix(body, []byte("data:")) {
		return extractUsageFromSSE(body)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return usageTokens{}, "", false
	}
	return parseUsageFromPayload(payload)
}

func extractUsageFromSSE(body []byte) (usageTokens, string, bool) {
	var merged usageTokens
	var model string
	found := false

	scanner := bufio.NewScanner(bytes.NewReader(body))
	buf := make([]byte, 0, 4096)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(strings.ToLower(line), "data:") {
			continue
		}
		chunk := strings.TrimSpace(line[len("data:"):])
		if chunk == "" || chunk == "[DONE]" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(chunk), &payload); err != nil {
			continue
		}
		tokens, m, ok := parseUsageFromPayload(payload)
		if !ok {
			continue
		}
		// Accumulate: keep the maximum of each field across all SSE events.
		// Claude splits usage across message_start (input_tokens) and
		// message_delta (output_tokens), so we must merge them.
		if tokens.InputTokens > merged.InputTokens {
			merged.InputTokens = tokens.InputTokens
		}
		if tokens.OutputTokens > merged.OutputTokens {
			merged.OutputTokens = tokens.OutputTokens
		}
		if tokens.CachedTokens > merged.CachedTokens {
			merged.CachedTokens = tokens.CachedTokens
		}
		if m != "" {
			model = m
		}
		found = true
	}

	if !found {
		return usageTokens{}, "", false
	}
	return merged, model, true
}

func parseUsageFromPayload(payload map[string]any) (usageTokens, string, bool) {
	if payload == nil {
		return usageTokens{}, "", false
	}

	model := strings.ToLower(readString(payload["model"]))
	// Gemini uses "modelVersion" instead of "model".
	if model == "" {
		model = strings.ToLower(readString(payload["modelVersion"]))
	}

	if usageMap, ok := payload["usage"].(map[string]any); ok {
		tokens, found := parseUsageMap(usageMap)
		if found {
			return tokens, model, true
		}
	}

	// Gemini native API: usageMetadata with promptTokenCount / candidatesTokenCount.
	if usageMap, ok := payload["usageMetadata"].(map[string]any); ok {
		tokens, found := parseUsageMap(usageMap)
		if found {
			return tokens, model, true
		}
	}

	if responseMap, ok := payload["response"].(map[string]any); ok {
		if model == "" {
			model = strings.ToLower(readString(responseMap["model"]))
		}
		if usageMap, ok := responseMap["usage"].(map[string]any); ok {
			tokens, found := parseUsageMap(usageMap)
			if found {
				return tokens, model, true
			}
		}
	}

	// Claude SSE message_start event: usage nested under "message.usage".
	if messageMap, ok := payload["message"].(map[string]any); ok {
		if model == "" {
			model = strings.ToLower(readString(messageMap["model"]))
		}
		if usageMap, ok := messageMap["usage"].(map[string]any); ok {
			tokens, found := parseUsageMap(usageMap)
			if found {
				return tokens, model, true
			}
		}
	}

	if _, hasPrompt := payload["prompt_tokens"]; hasPrompt {
		return usageTokens{
			InputTokens:  readInt64(payload["prompt_tokens"]),
			OutputTokens: readInt64(payload["completion_tokens"]),
		}, model, true
	}

	return usageTokens{}, model, false
}

func parseUsageMap(usageMap map[string]any) (usageTokens, bool) {
	if usageMap == nil {
		return usageTokens{}, false
	}

	hasAny := false
	readField := func(names ...string) (int64, bool) {
		for _, name := range names {
			if value, ok := usageMap[name]; ok {
				hasAny = true
				return readInt64(value), true
			}
		}
		return 0, false
	}

	input, _ := readField("input_tokens", "prompt_tokens", "promptTokenCount")
	output, _ := readField("output_tokens", "completion_tokens", "candidatesTokenCount")
	cached, hasCached := readField("cached_tokens", "cachedContentTokenCount")
	if !hasCached {
		if details, ok := usageMap["input_tokens_details"].(map[string]any); ok {
			if value, ok := details["cached_tokens"]; ok {
				hasAny = true
				cached = readInt64(value)
			}
		}
		if details, ok := usageMap["prompt_tokens_details"].(map[string]any); ok {
			if value, ok := details["cached_tokens"]; ok {
				hasAny = true
				cached = readInt64(value)
			}
		}
	}

	// Claude prompt caching: cache_read_input_tokens is the cached portion,
	// cache_creation_input_tokens is tokens used to build the cache entry.
	// Claude's "input_tokens" only counts non-cached tokens, so we add the
	// cache fields to get the true total and map them to our cached_tokens.
	cacheRead := readInt64(usageMap["cache_read_input_tokens"])
	cacheCreation := readInt64(usageMap["cache_creation_input_tokens"])
	if cacheRead > 0 || cacheCreation > 0 {
		hasAny = true
		input += cacheRead + cacheCreation
		if cacheRead > cached {
			cached = cacheRead
		}
	}

	if !hasAny {
		return usageTokens{}, false
	}

	return usageTokens{
		InputTokens:  input,
		OutputTokens: output,
		CachedTokens: cached,
	}, true
}

func readInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n
		}
		f, err := v.Float64()
		if err == nil {
			return int64(f)
		}
	case string:
		var n json.Number = json.Number(strings.TrimSpace(v))
		if iv, err := n.Int64(); err == nil {
			return iv
		}
	}
	return 0
}

func readString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

type limitedCaptureWriter struct {
	limit int
	buf   []byte
}

func (w *limitedCaptureWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	// 超过 limit 时只保留尾部 limit 字节，避免无限增长
	if len(w.buf) > w.limit {
		excess := len(w.buf) - w.limit
		copy(w.buf, w.buf[excess:])
		w.buf = w.buf[:w.limit]
	}
	return len(p), nil
}

func (w *limitedCaptureWriter) Bytes() []byte {
	if len(w.buf) == 0 {
		return nil
	}
	return w.buf
}
