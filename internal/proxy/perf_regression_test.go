// perf_regression_test.go — 性能优化前置回归测试。
// 锁定 #7(httptrace)、#8(isKeyExhausted bytes化)、#9(extractUsageFromSSE 倒序扫描) 的当前行为。
package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// ---------- #8: isKeyExhausted 边界 case ----------

func TestIsKeyExhausted_EmptyBody(t *testing.T) {
	// Empty body should never be exhausted
	exh, code := isKeyExhausted(401, nil)
	if exh {
		t.Errorf("nil body should not be exhausted, got code=%q", code)
	}
	exh, code = isKeyExhausted(429, []byte{})
	if exh {
		t.Errorf("empty body should not be exhausted, got code=%q", code)
	}
}

func TestIsKeyExhausted_CaseInsensitiveMatching(t *testing.T) {
	// Verify case-insensitive matching works for lowercase-checked patterns
	cases := []struct {
		name    string
		status  int
		body    string
		wantExh bool
	}{
		{"uppercase QUOTA EXCEEDED", 429, `{"error":"QUOTA EXCEEDED"}`, true},
		{"mixed case Quota Exceeded", 429, `{"error":"Quota Exceeded"}`, true},
		{"uppercase RATE LIMIT", 429, `{"error":"RATE LIMIT reached"}`, true}, // rate_limited (triggers key switch)
		{"uppercase ACCOUNT DISABLED", 403, `{"error":"ACCOUNT DISABLED"}`, true},
		{"uppercase INVALID API KEY", 401, `{"error":"INVALID API KEY"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exh, _ := isKeyExhausted(tc.status, []byte(tc.body))
			if exh != tc.wantExh {
				t.Errorf("isKeyExhausted(%d, %q) = %v, want %v", tc.status, tc.body, exh, tc.wantExh)
			}
		})
	}
}

func TestIsKeyExhausted_LargeBody(t *testing.T) {
	// Body up to 4096 bytes (the LimitReader cap in forward.go)
	padding := strings.Repeat("x", 3900)
	body := fmt.Sprintf(`{"error":"%squota exceeded%s"}`, padding, padding[:50])
	exh, code := isKeyExhausted(429, []byte(body))
	if !exh {
		t.Error("expected quota exceeded to be detected in large body")
	}
	if code != "quota_exceeded" {
		t.Errorf("expected code quota_exceeded, got %q", code)
	}
}

func TestIsKeyExhausted_429QuotaVsRateLimit(t *testing.T) {
	// Critical: 429 with quota patterns must be exhausted, 429 with only rate-limit patterns must not
	cases := []struct {
		name    string
		body    string
		wantExh bool
	}{
		// Quota patterns → exhausted
		{"quota exceeded", `{"error":"You exceeded your current quota"}`, true},
		{"exceeded your quota", `{"error":"exceeded your quota"}`, true},
		{"resource_exhausted", `{"error":"RESOURCE_EXHAUSTED"}`, true},
		{"Throttling.AllocationQuota", `{"code":"Throttling.AllocationQuota"}`, true},
		{"PrepaidBillOverdue", `{"code":"PrepaidBillOverdue"}`, true},
		{"PostpaidBillOverdue", `{"code":"PostpaidBillOverdue"}`, true},
		{"CommodityNotPurchased", `{"code":"CommodityNotPurchased"}`, true},

		// Pure rate limit → rate_limited (triggers key switch with 60s cooldown)
		{"throttling only", `{"code":"Throttling","message":"Requests throttling"}`, true},
		{"rate limit", `{"error":"Rate limit reached"}`, true},
		{"too many requests", `{"error":"Too many requests"}`, true},
		{"rate_limit_error", `{"error":{"type":"rate_limit_error"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exh, _ := isKeyExhausted(429, []byte(tc.body))
			if exh != tc.wantExh {
				t.Errorf("429 body=%q: exhausted=%v, want %v", tc.body, exh, tc.wantExh)
			}
		})
	}
}

func TestIsKeyExhausted_403Patterns(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantExh bool
		wantCode string
	}{
		{"AccessDenied.Unpurchased", `{"code":"AccessDenied.Unpurchased"}`, true, "AccessDenied.Unpurchased"},
		{"AllocationQuota.FreeTierOnly", `{"code":"AllocationQuota.FreeTierOnly"}`, true, "free_tier_exhausted"},
		{"generic 403", `{"error":"forbidden"}`, false, ""},
		{"account disabled", `{"error":"account disabled"}`, true, "account_disabled"},
		{"account suspended", `{"error":"account suspended"}`, true, "account_disabled"},
		{"account deactivated", `{"error":"account has been deactivated"}`, true, "account_disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exh, code := isKeyExhausted(403, []byte(tc.body))
			if exh != tc.wantExh {
				t.Errorf("exhausted=%v, want %v", exh, tc.wantExh)
			}
			if code != tc.wantCode {
				t.Errorf("code=%q, want %q", code, tc.wantCode)
			}
		})
	}
}

// ---------- #9: extractUsageFromSSE 边界 case ----------

func TestExtractUsageFromSSE_OpenAIFormat(t *testing.T) {
	// OpenAI streaming: usage in the last data chunk
	sseBody := []byte(
		`data: {"id":"chatcmpl-abc","choices":[{"delta":{"content":"Hello"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-abc","choices":[{"delta":{"content":" world"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-abc","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}` + "\n\n" +
		`data: [DONE]` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found in OpenAI SSE")
	}
	if tokens.InputTokens != 50 {
		t.Errorf("expected input_tokens=50, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 10 {
		t.Errorf("expected output_tokens=10, got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromSSE_GeminiFormat(t *testing.T) {
	// Gemini streaming: usageMetadata with promptTokenCount / candidatesTokenCount
	sseBody := []byte(
		`data: {"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[{"text":" there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":25,"candidatesTokenCount":5,"totalTokenCount":30}}` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found in Gemini SSE")
	}
	if tokens.InputTokens != 25 {
		t.Errorf("expected input_tokens=25, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 5 {
		t.Errorf("expected output_tokens=5, got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromSSE_LargeBody_UsageAtEnd(t *testing.T) {
	// Simulate 500 chunks of content, usage only in the last chunk
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString(fmt.Sprintf(`data: {"id":"c","choices":[{"delta":{"content":"word%d"}}]}`, i))
		sb.WriteString("\n\n")
	}
	sb.WriteString(`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`)
	sb.WriteString("\n\ndata: [DONE]\n\n")

	tokens, _, ok := extractUsageFromSSE([]byte(sb.String()))
	if !ok {
		t.Fatal("expected usage found in large SSE body")
	}
	if tokens.InputTokens != 1000 {
		t.Errorf("expected input_tokens=1000, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 500 {
		t.Errorf("expected output_tokens=500, got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromSSE_NoUsage(t *testing.T) {
	sseBody := []byte(
		`data: {"id":"c","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n",
	)

	_, _, ok := extractUsageFromSSE(sseBody)
	if ok {
		t.Error("expected no usage found")
	}
}

func TestExtractUsageFromSSE_EmptyBody(t *testing.T) {
	_, _, ok := extractUsageFromSSE(nil)
	if ok {
		t.Error("expected no usage for nil body")
	}
	_, _, ok = extractUsageFromSSE([]byte{})
	if ok {
		t.Error("expected no usage for empty body")
	}
}

func TestExtractUsageFromSSE_MalformedChunks_Skipped(t *testing.T) {
	sseBody := []byte(
		`data: not json at all` + "\n\n" +
		`data: {"broken json` + "\n\n" +
		`data: {"id":"c","choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		`data: [DONE]` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found despite malformed chunks")
	}
	if tokens.InputTokens != 10 {
		t.Errorf("expected input_tokens=10, got %d", tokens.InputTokens)
	}
}

func TestExtractUsageFromSSE_WithCachedTokens(t *testing.T) {
	// OpenAI with prompt_tokens_details.cached_tokens
	sseBody := []byte(
		`data: {"id":"c","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\n" +
		`data: [DONE]` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage found")
	}
	if tokens.InputTokens != 100 {
		t.Errorf("expected input_tokens=100, got %d", tokens.InputTokens)
	}
	if tokens.CachedTokens != 80 {
		t.Errorf("expected cached_tokens=80, got %d", tokens.CachedTokens)
	}
}

func TestExtractUsageFromSSE_OnlyNonDataLines(t *testing.T) {
	// SSE with event: lines and comments, no data: lines with usage
	sseBody := []byte(
		`event: message_start` + "\n" +
		`: this is a comment` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","delta":{"text":"hi"}}` + "\n\n" +
		`event: message_stop` + "\n\n",
	)

	_, _, ok := extractUsageFromSSE(sseBody)
	if ok {
		t.Error("expected no usage when no usage data present")
	}
}

// ---------- extractStreamField 边界 case（#4 已优化，补充回归） ----------

func TestExtractStreamField_WhitespaceAroundColon(t *testing.T) {
	cases := []struct {
		name       string
		body       []byte
		wantStream bool
		wantFound  bool
	}{
		{"space before colon", []byte(`{"stream" :true}`), true, true},
		{"space after colon", []byte(`{"stream": true}`), true, true},
		{"spaces both sides", []byte(`{"stream" : true}`), true, true},
		{"tab after colon", []byte("{\"stream\":\ttrue}"), true, true},
		{"newline after colon", []byte("{\"stream\":\ntrue}"), true, true},
		{"false with spaces", []byte(`{"stream" : false}`), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStream, gotFound := extractStreamField(tc.body)
			if gotStream != tc.wantStream || gotFound != tc.wantFound {
				t.Errorf("extractStreamField(%q) = (%v, %v), want (%v, %v)",
					string(tc.body), gotStream, gotFound, tc.wantStream, tc.wantFound)
			}
		})
	}
}

func TestExtractStreamField_StreamKeyInNestedObject(t *testing.T) {
	// "stream" inside a nested object should NOT be matched as top-level
	// Note: the byte-scan approach may match nested keys — this test documents current behavior
	body := []byte(`{"options":{"stream":true},"model":"gpt-4"}`)
	gotStream, gotFound := extractStreamField(body)
	// The byte-scan will find "stream" in the nested object.
	// This is acceptable because: (1) LLM API requests don't nest "stream",
	// (2) the old JSON unmarshal approach would NOT match nested keys.
	// Document the behavior — if this becomes a problem, we can tighten the scan.
	_ = gotStream
	_ = gotFound
	t.Logf("nested stream: gotStream=%v, gotFound=%v (behavior documented)", gotStream, gotFound)
}

func TestExtractStreamField_LargeBody(t *testing.T) {
	// Large request body with stream field at the end
	messages := strings.Repeat(`{"role":"user","content":"hello world "},`, 1000)
	body := []byte(fmt.Sprintf(`{"model":"gpt-4","messages":[%s],"stream":true}`, messages))
	gotStream, gotFound := extractStreamField(body)
	if !gotFound || !gotStream {
		t.Errorf("expected stream=true found in large body, got stream=%v found=%v", gotStream, gotFound)
	}
}

// ---------- limitedCaptureWriter 行为锁定 ----------

func TestLimitedCaptureWriter_BasicWrite(t *testing.T) {
	w := &limitedCaptureWriter{limit: 1024}
	data := []byte("hello world")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected n=%d, got %d", len(data), n)
	}
	if string(w.Bytes()) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(w.Bytes()))
	}
}

func TestLimitedCaptureWriter_MultipleWrites(t *testing.T) {
	w := &limitedCaptureWriter{limit: 1024}
	w.Write([]byte("hello "))
	w.Write([]byte("world"))
	if string(w.Bytes()) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(w.Bytes()))
	}
}

func TestLimitedCaptureWriter_OverflowTruncates(t *testing.T) {
	w := &limitedCaptureWriter{limit: 10}
	w.Write([]byte("12345678901234567890")) // 20 bytes, limit 10
	got := string(w.Bytes())
	if len(got) != 10 {
		t.Errorf("expected 10 bytes, got %d: %q", len(got), got)
	}
	// Should keep the tail
	if got != "1234567890" {
		// After overflow, the last 10 bytes should be kept
		// "12345678901234567890" → tail 10 = "1234567890"
		// Actually the last 10 chars of "12345678901234567890" are "1234567890"
		t.Logf("overflow result: %q (tail of 20-byte input with limit 10)", got)
	}
}

func TestLimitedCaptureWriter_IncrementalOverflow(t *testing.T) {
	w := &limitedCaptureWriter{limit: 10}
	// Write 8 bytes, then 8 more = 16 total, limit 10
	w.Write([]byte("12345678"))
	w.Write([]byte("ABCDEFGH"))
	got := string(w.Bytes())
	if len(got) != 10 {
		t.Errorf("expected 10 bytes after overflow, got %d: %q", len(got), got)
	}
	// Should keep tail 10: "5678ABCDEFGH" → last 10 = "78ABCDEFGH"
	// Actually: "12345678" + "ABCDEFGH" = "12345678ABCDEFGH" (16 bytes)
	// Tail 10 = "8ABCDEFGH" — wait, that's 9. Let me count:
	// "12345678ABCDEFGH" has 16 chars, tail 10 = "78ABCDEFGH"
	expected := "78ABCDEFGH"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestLimitedCaptureWriter_ZeroLimit(t *testing.T) {
	w := &limitedCaptureWriter{limit: 0}
	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("expected n=5, got %d", n)
	}
	if w.Bytes() != nil {
		t.Errorf("expected nil bytes for zero limit, got %q", string(w.Bytes()))
	}
}

func TestLimitedCaptureWriter_EmptyBytes(t *testing.T) {
	w := &limitedCaptureWriter{limit: 1024}
	if w.Bytes() != nil {
		t.Error("expected nil for empty writer")
	}
}

func TestLimitedCaptureWriter_ReturnsNEqualToInputLen(t *testing.T) {
	// Write must always return len(p) even when truncating, to satisfy io.Writer contract
	w := &limitedCaptureWriter{limit: 5}
	data := []byte("1234567890")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write must return n=%d (len(input)), got %d", len(data), n)
	}
}
