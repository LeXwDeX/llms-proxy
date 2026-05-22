// sanitize.go — 请求净化（Azure 字段白名单）与模型名提取/验证。
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

var (
	azureChatCompletionsFieldWhitelist = toFieldSet(
		"audio",
		"data_sources",
		"frequency_penalty",
		"function_call",
		"functions",
		"logit_bias",
		"logprobs",
		"max_completion_tokens",
		"max_tokens",
		"messages",
		"metadata",
		"modalities",
		"model",
		"n",
		"parallel_tool_calls",
		"prediction",
		"presence_penalty",
		"prompt_cache_key",
		"reasoning_effort",
		"response_format",
		"seed",
		"stop",
		"store",
		"stream",
		"stream_options",
		"temperature",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"user",
		"user_security_context",
	)
	azureResponsesFieldWhitelist = toFieldSet(
		"background",
		"include",
		"input",
		"instructions",
		"max_output_tokens",
		"max_tool_calls",
		"metadata",
		"model",
		"parallel_tool_calls",
		"previous_response_id",
		"prompt",
		"prompt_cache_key",
		"reasoning",
		"store",
		"stream",
		"temperature",
		"text",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"truncation",
		"user",
	)
	azureEmbeddingsFieldWhitelist = toFieldSet(
		"dimensions",
		"encoding_format",
		"input",
		"model",
		"user",
	)
)

func toFieldSet(fields ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		field = strings.ToLower(strings.TrimSpace(field))
		if field == "" {
			continue
		}
		set[field] = struct{}{}
	}
	return set
}

func whitelistForPath(path string) (map[string]struct{}, bool) {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return nil, false
	}

	if strings.HasSuffix(path, "/chat/completions") {
		return azureChatCompletionsFieldWhitelist, true
	}
	if strings.HasSuffix(path, "/responses") {
		return azureResponsesFieldWhitelist, true
	}
	if strings.HasSuffix(path, "/embeddings") {
		return azureEmbeddingsFieldWhitelist, true
	}

	return nil, false
}

func sanitizeRequestBodyForAzure(r *http.Request, body []byte) ([]byte, []string) {
	if r == nil || len(body) == 0 {
		return body, nil
	}

	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return body, nil
	}

	fieldWhitelist, ok := whitelistForPath(r.URL.Path)
	if !ok {
		return body, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil
	}

	stripped := make([]string, 0)
	for key := range payload {
		keyLower := strings.ToLower(strings.TrimSpace(key))
		if _, allowed := fieldWhitelist[keyLower]; allowed {
			continue
		}
		delete(payload, key)
		stripped = append(stripped, key)
	}

	if len(stripped) == 0 {
		return body, nil
	}

	filtered, err := json.Marshal(payload)
	if err != nil {
		return body, nil
	}

	sort.Strings(stripped)
	return filtered, stripped
}

func extractModel(r *http.Request, body []byte) string {
	if r == nil {
		return ""
	}

	pathLower := strings.ToLower(r.URL.Path)
	if isListEndpoint(pathLower) {
		return ""
	}

	path := strings.ToLower(r.URL.Path)
	const deploymentsSegment = "/deployments/"
	if idx := strings.Index(path, deploymentsSegment); idx >= 0 {
		after := path[idx+len(deploymentsSegment):]
		if slash := strings.Index(after, "/"); slash >= 0 {
			after = after[:slash]
		}
		return strings.TrimSpace(after)
	}

	// Gemini native REST API: /v1beta/models/{model}:{action}
	// Also match /v1alpha/models/... and /v1/models/... variants.
	const modelsSegment = "/models/"
	if idx := strings.Index(path, modelsSegment); idx >= 0 {
		after := path[idx+len(modelsSegment):]
		// Strip :{action} suffix (e.g. ":generatecontent", ":streamgeneratecontent")
		if colon := strings.Index(after, ":"); colon >= 0 {
			after = after[:colon]
		}
		// Strip any trailing path segments
		if slash := strings.Index(after, "/"); slash >= 0 {
			after = after[:slash]
		}
		if m := strings.TrimSpace(after); m != "" {
			return m
		}
	}

	if len(body) > 0 {
		contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if strings.Contains(contentType, "application/x-www-form-urlencoded") {
			if vals, err := url.ParseQuery(string(body)); err == nil {
				if model := strings.TrimSpace(vals.Get("model")); model != "" {
					return model
				}
			}
		}

		if strings.Contains(contentType, "multipart/form-data") {
			if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil {
				if boundary := strings.TrimSpace(params["boundary"]); boundary != "" {
					if model := extractMultipartModel(body, boundary); model != "" {
						return model
					}
				}
			}
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			if m, ok := payload["model"].(string); ok {
				return strings.TrimSpace(m)
			}
		}
	}
	return ""
}

func extractMultipartModel(body []byte, boundary string) string {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return ""
		}
		if err != nil {
			return ""
		}

		if strings.EqualFold(part.FormName(), "model") {
			data, err := io.ReadAll(io.LimitReader(part, 1024))
			_ = part.Close()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(data))
		}

		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
}

func modelAllowed(t *Target, model string) bool {
	if t == nil {
		return false
	}
	if len(t.AllowedModels) == 0 {
		return true
	}
	modelKey := strings.ToLower(strings.TrimSpace(model))
	if modelKey == "" {
		return false
	}
	if t.allowedModelsSet != nil {
		_, ok := t.allowedModelsSet[modelKey]
		return ok
	}
	for _, m := range t.AllowedModels {
		if strings.EqualFold(m, modelKey) {
			return true
		}
	}
	return false
}

func (s *Service) anyTargetRequiresModel() bool {
	snapshot := s.targetSnapshot()
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		if len(state.Target().AllowedModels) > 0 {
			return true
		}
	}
	return false
}

func (s *Service) ensureModelAllowed(target *Target, model string) error {
	if target == nil || len(target.AllowedModels) == 0 {
		return nil
	}

	if model == "" {
		return errors.New("model required when allowed_models is configured")
	}
	if modelAllowed(target, model) {
		return nil
	}
	return fmt.Errorf("model %q not allowed for target %q", model, target.Name)
}

func normalizeAllowed(list []string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, item := range list {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		m[item] = struct{}{}
	}
	return m
}
