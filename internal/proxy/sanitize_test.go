// sanitize_test.go — sanitizeRequestBodyForAzure / ensureModelAllowed / normalizeAllowed 单元测试。
// 为后续性能优化（延迟 Azure sanitize、ensureModelAllowed 传参、normalizeAllowed 缓存）提供行为锁定。
package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------- sanitizeRequestBodyForAzure ----------

func TestSanitizeRequestBodyForAzure_ChatCompletions_StripsUnknownFields(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"custom_field":"remove-me","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) == 0 {
		t.Fatal("expected some fields to be stripped")
	}

	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Whitelisted fields must be preserved
	if _, ok := parsed["model"]; !ok {
		t.Error("expected 'model' to be preserved")
	}
	if _, ok := parsed["messages"]; !ok {
		t.Error("expected 'messages' to be preserved")
	}
	if _, ok := parsed["temperature"]; !ok {
		t.Error("expected 'temperature' to be preserved")
	}

	// Non-whitelisted fields must be stripped
	if _, ok := parsed["custom_field"]; ok {
		t.Error("expected 'custom_field' to be stripped")
	}
	if _, ok := parsed["foo"]; ok {
		t.Error("expected 'foo' to be stripped")
	}
}

func TestSanitizeRequestBodyForAzure_Responses_StripsUnknownFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.2","input":"hello","prompt_cache_key":"sess-a","prompt_cache_retention":"24h","unknown":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) == 0 {
		t.Fatal("expected some fields to be stripped")
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)

	// Whitelisted
	if _, ok := parsed["model"]; !ok {
		t.Error("expected 'model' preserved")
	}
	if _, ok := parsed["input"]; !ok {
		t.Error("expected 'input' preserved")
	}
	if _, ok := parsed["prompt_cache_key"]; !ok {
		t.Error("expected 'prompt_cache_key' preserved")
	}

	// Stripped
	if _, ok := parsed["prompt_cache_retention"]; ok {
		t.Error("expected 'prompt_cache_retention' stripped")
	}
	if _, ok := parsed["unknown"]; ok {
		t.Error("expected 'unknown' stripped")
	}
}

func TestSanitizeRequestBodyForAzure_Embeddings_StripsUnknownFields(t *testing.T) {
	body := []byte(`{"model":"text-embedding-3-small","input":"hello","dimensions":512,"extra":"remove"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) == 0 {
		t.Fatal("expected some fields to be stripped")
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)

	if _, ok := parsed["model"]; !ok {
		t.Error("expected 'model' preserved")
	}
	if _, ok := parsed["input"]; !ok {
		t.Error("expected 'input' preserved")
	}
	if _, ok := parsed["dimensions"]; !ok {
		t.Error("expected 'dimensions' preserved")
	}
	if _, ok := parsed["extra"]; ok {
		t.Error("expected 'extra' stripped")
	}
}

func TestSanitizeRequestBodyForAzure_NonMatchingPath_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","custom":"keep"}`)
	paths := []string{
		"/v1/images/generations",
		"/v1/audio/transcriptions",
		"/v1/models",
		"/v1/files",
		"/some/random/path",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		result, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) != 0 {
			t.Errorf("path %q: expected no stripping, got %v", p, stripped)
		}
		if string(result) != string(body) {
			t.Errorf("path %q: expected original body returned", p)
		}
	}
}

func TestSanitizeRequestBodyForAzure_NonModifyingMethod_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","custom":"keep"}`)
	methods := []string{http.MethodGet, http.MethodDelete, http.MethodHead, http.MethodOptions}
	for _, m := range methods {
		req := httptest.NewRequest(m, "/v1/chat/completions", nil)
		result, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) != 0 {
			t.Errorf("method %q: expected no stripping, got %v", m, stripped)
		}
		if string(result) != string(body) {
			t.Errorf("method %q: expected original body returned", m)
		}
	}
}

func TestSanitizeRequestBodyForAzure_AllFieldsWhitelisted_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[],"temperature":0.5,"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) != 0 {
		t.Errorf("expected no stripping when all fields whitelisted, got %v", stripped)
	}
	// When nothing stripped, should return original body (not re-marshaled)
	if string(result) != string(body) {
		t.Error("expected original body when nothing stripped")
	}
}

func TestSanitizeRequestBodyForAzure_EmptyBody_ReturnsOriginal(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	result, stripped := sanitizeRequestBodyForAzure(req, nil)
	if len(stripped) != 0 {
		t.Error("expected no stripping for nil body")
	}
	if result != nil {
		t.Error("expected nil result for nil body")
	}

	result, stripped = sanitizeRequestBodyForAzure(req, []byte{})
	if len(stripped) != 0 {
		t.Error("expected no stripping for empty body")
	}
}

func TestSanitizeRequestBodyForAzure_NilRequest_ReturnsOriginal(t *testing.T) {
	body := []byte(`{"model":"gpt-4o"}`)
	result, stripped := sanitizeRequestBodyForAzure(nil, body)
	if len(stripped) != 0 {
		t.Error("expected no stripping for nil request")
	}
	if string(result) != string(body) {
		t.Error("expected original body for nil request")
	}
}

func TestSanitizeRequestBodyForAzure_InvalidJSON_ReturnsOriginal(t *testing.T) {
	body := []byte(`this is not json at all`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) != 0 {
		t.Error("expected no stripping for invalid JSON")
	}
	if string(result) != string(body) {
		t.Error("expected original body for invalid JSON")
	}
}

func TestSanitizeRequestBodyForAzure_PUT_Patch_AlsoSanitize(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[],"custom":"remove"}`)
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		req := httptest.NewRequest(method, "/v1/chat/completions", nil)
		result, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) == 0 {
			t.Errorf("method %q: expected stripping", method)
		}
		var parsed map[string]any
		json.Unmarshal(result, &parsed)
		if _, ok := parsed["custom"]; ok {
			t.Errorf("method %q: expected 'custom' stripped", method)
		}
	}
}

func TestSanitizeRequestBodyForAzure_ChatCompletions_FullWhitelist(t *testing.T) {
	// Verify all documented whitelist fields are actually preserved
	allFields := map[string]any{
		"audio":                nil,
		"data_sources":         nil,
		"frequency_penalty":    0.5,
		"function_call":        "auto",
		"functions":            []any{},
		"logit_bias":           map[string]any{},
		"logprobs":             false,
		"max_completion_tokens": 100,
		"max_tokens":           200,
		"messages":             []any{},
		"metadata":             map[string]any{},
		"modalities":           []any{"text"},
		"model":                "gpt-4o",
		"n":                    1,
		"parallel_tool_calls":  true,
		"prediction":           map[string]any{},
		"presence_penalty":     0.0,
		"prompt_cache_key":     "sess",
		"reasoning_effort":     "medium",
		"response_format":      map[string]any{},
		"seed":                 42,
		"stop":                 "END",
		"store":                false,
		"stream":               true,
		"stream_options":       map[string]any{},
		"temperature":          0.7,
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_logprobs":         5,
		"top_p":                0.9,
		"user":                 "test-user",
		"user_security_context": "ctx",
	}
	body, _ := json.Marshal(allFields)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	result, stripped := sanitizeRequestBodyForAzure(req, body)
	if len(stripped) != 0 {
		t.Errorf("expected all fields whitelisted, but stripped: %v", stripped)
	}

	var parsed map[string]any
	json.Unmarshal(result, &parsed)
	for key := range allFields {
		if _, ok := parsed[key]; !ok {
			t.Errorf("expected whitelisted field %q to be preserved", key)
		}
	}
}

func TestSanitizeRequestBodyForAzure_DeploymentPath_Matches(t *testing.T) {
	// Azure deployment paths like /openai/deployments/xxx/chat/completions should also match
	body := []byte(`{"model":"gpt-4o","messages":[],"custom":"remove"}`)
	paths := []string{
		"/openai/deployments/gpt-4o/chat/completions",
		"/openai/deployments/gpt-4o/responses",
		"/openai/deployments/text-embedding-3-small/embeddings",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		_, stripped := sanitizeRequestBodyForAzure(req, body)
		if len(stripped) == 0 {
			t.Errorf("path %q: expected stripping", p)
		}
	}
}

// ---------- ensureModelAllowed ----------

func TestEnsureModelAllowed_NoRestrictions(t *testing.T) {
	s := &Service{}
	target := &Target{Name: "t1", AllowedModels: nil}
	if err := s.ensureModelAllowed(target, "gpt-4o"); err != nil {
		t.Errorf("expected nil error for no restrictions, got %v", err)
	}
}

func TestEnsureModelAllowed_NilTarget(t *testing.T) {
	s := &Service{}
	if err := s.ensureModelAllowed(nil, "gpt-4o"); err != nil {
		t.Errorf("expected nil error for nil target, got %v", err)
	}
}

func TestEnsureModelAllowed_ModelAllowed(t *testing.T) {
	s := &Service{}
	target := &Target{
		Name:            "t1",
		AllowedModels:   []string{"gpt-4o", "gpt-3.5-turbo"},
		allowedModelsSet: map[string]struct{}{"gpt-4o": {}, "gpt-3.5-turbo": {}},
	}
	if err := s.ensureModelAllowed(target, "gpt-4o"); err != nil {
		t.Errorf("expected nil error for allowed model, got %v", err)
	}
}

func TestEnsureModelAllowed_ModelNotAllowed(t *testing.T) {
	s := &Service{}
	target := &Target{
		Name:            "t1",
		AllowedModels:   []string{"gpt-4o"},
		allowedModelsSet: map[string]struct{}{"gpt-4o": {}},
	}
	err := s.ensureModelAllowed(target, "gpt-3.5-turbo")
	if err == nil {
		t.Fatal("expected error for disallowed model")
	}
}

func TestEnsureModelAllowed_EmptyModel_WithRestrictions(t *testing.T) {
	s := &Service{}
	target := &Target{
		Name:            "t1",
		AllowedModels:   []string{"gpt-4o"},
		allowedModelsSet: map[string]struct{}{"gpt-4o": {}},
	}
	err := s.ensureModelAllowed(target, "")
	if err == nil {
		t.Fatal("expected error for empty model with restrictions")
	}
}

// ---------- normalizeAllowed ----------

func TestNormalizeAllowed_Basic(t *testing.T) {
	result := normalizeAllowed([]string{"Primary", "SECONDARY", "tertiary"})
	expected := []string{"primary", "secondary", "tertiary"}
	for _, e := range expected {
		if _, ok := result[e]; !ok {
			t.Errorf("expected %q in normalized set", e)
		}
	}
	if len(result) != 3 {
		t.Errorf("expected 3 entries, got %d", len(result))
	}
}

func TestNormalizeAllowed_EmptyList(t *testing.T) {
	result := normalizeAllowed([]string{})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestNormalizeAllowed_SkipsEmptyStrings(t *testing.T) {
	result := normalizeAllowed([]string{"", "  ", "valid"})
	if len(result) != 1 {
		t.Errorf("expected 1 entry, got %d", len(result))
	}
	if _, ok := result["valid"]; !ok {
		t.Error("expected 'valid' in result")
	}
}

func TestNormalizeAllowed_TrimsWhitespace(t *testing.T) {
	result := normalizeAllowed([]string{"  target-a  ", "target-b"})
	if _, ok := result["target-a"]; !ok {
		t.Error("expected 'target-a' (trimmed) in result")
	}
	if _, ok := result["target-b"]; !ok {
		t.Error("expected 'target-b' in result")
	}
}

// ---------- modelAllowed ----------

func TestModelAllowed_NilTarget(t *testing.T) {
	if modelAllowed(nil, "gpt-4o") {
		t.Error("expected false for nil target")
	}
}

func TestModelAllowed_NoRestrictions(t *testing.T) {
	target := &Target{Name: "t1"}
	if !modelAllowed(target, "any-model") {
		t.Error("expected true when no restrictions")
	}
}

func TestModelAllowed_EmptyModel_WithRestrictions(t *testing.T) {
	target := &Target{
		Name:            "t1",
		AllowedModels:   []string{"gpt-4o"},
		allowedModelsSet: map[string]struct{}{"gpt-4o": {}},
	}
	if modelAllowed(target, "") {
		t.Error("expected false for empty model with restrictions")
	}
}

func TestModelAllowed_CaseInsensitive(t *testing.T) {
	target := &Target{
		Name:            "t1",
		AllowedModels:   []string{"GPT-4o"},
		allowedModelsSet: map[string]struct{}{"gpt-4o": {}},
	}
	if !modelAllowed(target, "gpt-4o") {
		t.Error("expected case-insensitive match")
	}
}

func TestModelAllowed_FallbackToLinearScan(t *testing.T) {
	// When allowedModelsSet is nil, should fall back to linear scan
	target := &Target{
		Name:          "t1",
		AllowedModels: []string{"gpt-4o", "gpt-3.5-turbo"},
	}
	if !modelAllowed(target, "gpt-4o") {
		t.Error("expected match via linear scan")
	}
	if modelAllowed(target, "gpt-5") {
		t.Error("expected no match via linear scan")
	}
}

// ---------------------------------------------------------------------------
// injectCacheControl
// ---------------------------------------------------------------------------

func TestInjectCacheControl_SystemMessage(t *testing.T) {
	body := []byte(`{"model":"qwen3.7-max","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hello"}]}`)
	result := injectCacheControl(body, "system", "second_to_last")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	msgs := payload["messages"].([]any)
	sysMsg := msgs[0].(map[string]any)

	// system message content should be converted to array with cache_control
	contentArr, ok := sysMsg["content"].([]any)
	if !ok {
		t.Fatal("expected content to be array after injection")
	}
	lastBlock := contentArr[len(contentArr)-1].(map[string]any)
	cc, ok := lastBlock["cache_control"].(map[string]any)
	if !ok {
		t.Fatal("expected cache_control on system message last block")
	}
	if cc["type"] != "ephemeral" {
		t.Errorf("expected ephemeral, got %v", cc["type"])
	}
}

func TestInjectCacheControl_NoSystemMessage_FallbackSecondToLast(t *testing.T) {
	body := []byte(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":"Hi"},{"role":"user","content":"How are you?"}]}`)
	result := injectCacheControl(body, "system", "second_to_last")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	msgs := payload["messages"].([]any)

	// Should inject into second-to-last message (assistant)
	assistantMsg := msgs[len(msgs)-2].(map[string]any)
	contentArr, ok := assistantMsg["content"].([]any)
	if !ok {
		t.Fatal("expected content to be array after injection")
	}
	lastBlock := contentArr[len(contentArr)-1].(map[string]any)
	if _, ok := lastBlock["cache_control"]; !ok {
		t.Fatal("expected cache_control on second-to-last message")
	}

	// Last message should NOT have cache_control
	lastMsg := msgs[len(msgs)-1].(map[string]any)
	if _, ok := lastMsg["content"].(string); !ok {
		t.Error("last message content should remain string")
	}
}

func TestInjectCacheControl_NoSystemMessage_FallbackNone(t *testing.T) {
	body := []byte(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":"Hi"},{"role":"user","content":"How are you?"}]}`)
	result := injectCacheControl(body, "system", "none")

	// Should return unchanged (no fallback)
	if string(result) != string(body) {
		t.Error("should not modify body when fallback is none and role not found")
	}
}

func TestInjectCacheControl_CustomRole(t *testing.T) {
	body := []byte(`{"model":"test","messages":[{"role":"system","content":"System"},{"role":"developer","content":"Developer instructions"},{"role":"user","content":"Hello"}]}`)
	result := injectCacheControl(body, "developer", "none")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	msgs := payload["messages"].([]any)

	// Should inject into developer message, not system
	devMsg := msgs[1].(map[string]any)
	contentArr, ok := devMsg["content"].([]any)
	if !ok {
		t.Fatal("expected developer content to be array after injection")
	}
	lastBlock := contentArr[len(contentArr)-1].(map[string]any)
	if _, ok := lastBlock["cache_control"]; !ok {
		t.Fatal("expected cache_control on developer message")
	}

	// System message should NOT have cache_control
	sysMsg := msgs[0].(map[string]any)
	if _, ok := sysMsg["content"].(string); !ok {
		t.Error("system message content should remain string")
	}
}

func TestInjectCacheControl_AlreadyHasCacheControl(t *testing.T) {
	body := []byte(`{"model":"qwen3.7-max","messages":[{"role":"system","content":[{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"Hello"}]}`)
	result := injectCacheControl(body, "system", "second_to_last")

	// Should return unchanged (no double injection)
	if string(result) != string(body) {
		t.Error("should not modify body that already has cache_control")
	}
}

func TestInjectCacheControl_ArrayContent(t *testing.T) {
	body := []byte(`{"model":"qwen3.7-max","messages":[{"role":"system","content":[{"type":"text","text":"Part 1"},{"type":"text","text":"Part 2"}]},{"role":"user","content":"Hello"}]}`)
	result := injectCacheControl(body, "system", "second_to_last")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	msgs := payload["messages"].([]any)
	sysMsg := msgs[0].(map[string]any)
	contentArr := sysMsg["content"].([]any)

	// cache_control should be on the LAST block only
	lastBlock := contentArr[len(contentArr)-1].(map[string]any)
	if _, ok := lastBlock["cache_control"]; !ok {
		t.Fatal("expected cache_control on last content block")
	}
	firstBlock := contentArr[0].(map[string]any)
	if _, ok := firstBlock["cache_control"]; ok {
		t.Error("first block should NOT have cache_control")
	}
}

func TestInjectCacheControl_EmptyMessages(t *testing.T) {
	body := []byte(`{"model":"qwen3.7-max","messages":[]}`)
	result := injectCacheControl(body, "system", "second_to_last")
	// Should return unchanged
	if string(result) != string(body) {
		t.Error("should not modify body with empty messages")
	}
}

func TestInjectCacheControl_InvalidJSON(t *testing.T) {
	body := []byte(`not json`)
	result := injectCacheControl(body, "system", "second_to_last")
	// Should return unchanged
	if string(result) != string(body) {
		t.Error("should return original body on invalid JSON")
	}
}

func TestInjectCacheControl_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"qwen3.7-max","temperature":0.7,"max_tokens":100,"messages":[{"role":"system","content":"Hi"},{"role":"user","content":"Hello"}]}`)
	result := injectCacheControl(body, "system", "second_to_last")

	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload["model"] != "qwen3.7-max" {
		t.Error("model should be preserved")
	}
	if payload["temperature"] != 0.7 {
		t.Error("temperature should be preserved")
	}
	if payload["max_tokens"] != float64(100) {
		t.Error("max_tokens should be preserved")
	}
}
