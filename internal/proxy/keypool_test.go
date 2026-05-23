package proxy

import (
	"log/slog"
	"testing"
)

func newTestKeyPool(keys []string) *keyPool {
	return newKeyPool("test-target", keys, 1800, slog.Default())
}

func TestSelectKeyForClient_Affinity(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b", "key-c", "key-d"})

	// 同一 client 应该始终返回同一个 key
	key1, idx1 := pool.selectKeyForClient("alice")
	key2, idx2 := pool.selectKeyForClient("alice")
	if key1 != key2 || idx1 != idx2 {
		t.Errorf("same client should get same key: got (%s,%d) and (%s,%d)", key1, idx1, key2, idx2)
	}

	// 不同 client 可能得到不同 key（不强制，但 hash 应该分散）
	keyBob, _ := pool.selectKeyForClient("bob")
	_ = keyBob // 不强制不同，只验证不 panic
}

func TestSelectKeyForClient_EmptyClient(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b"})

	// 空 clientName 退化为 selectKey（顺序消费，返回第一个）
	key, idx := pool.selectKeyForClient("")
	if key != "key-a" || idx != 0 {
		t.Errorf("empty client should get first key, got (%s, %d)", key, idx)
	}
}

func TestSelectKeyForClient_Overflow(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b", "key-c"})

	// 找到 alice 的亲和 key
	_, preferredIdx := pool.selectKeyForClient("alice")

	// 标记亲和 key 为 exhausted
	pool.markExhausted(preferredIdx, "quota_exceeded")

	// 应该溢出到下一个 active key
	key, idx := pool.selectKeyForClient("alice")
	if key == "" || idx == -1 {
		t.Fatal("should overflow to next active key")
	}
	if idx == preferredIdx {
		t.Errorf("should not return exhausted preferred key, got idx=%d", idx)
	}
}

func TestSelectKeyForClient_AllExhausted(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b"})

	pool.markExhausted(0, "quota_exceeded")
	pool.markExhausted(1, "quota_exceeded")

	key, idx := pool.selectKeyForClient("alice")
	if key != "" || idx != -1 {
		t.Errorf("all exhausted should return empty, got (%s, %d)", key, idx)
	}
}

func TestSelectKeyForClient_Deterministic(t *testing.T) {
	// 多次创建 pool，同一 client 应该映射到同一 key
	for i := 0; i < 10; i++ {
		pool := newTestKeyPool([]string{"key-0", "key-1", "key-2", "key-3", "key-4"})
		_, idx := pool.selectKeyForClient("test-user")
		expected := int(hashClientKey("test-user", "test-target") % 5)
		if idx != expected {
			t.Errorf("iteration %d: expected idx=%d, got %d", i, expected, idx)
		}
	}
}

func TestSelectKeyForClient_Distribution(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2", "key-3", "key-4"})

	counts := make(map[int]int)
	clients := []string{
		"alice", "bob", "charlie", "dave", "eve",
		"frank", "grace", "heidi", "ivan", "judy",
		"mallory", "oscar", "peggy", "trent", "victor",
		"wendy", "xavier", "yvonne", "zoe", "admin",
	}

	for _, c := range clients {
		_, idx := pool.selectKeyForClient(c)
		counts[idx]++
	}

	// 20 个 client 分到 5 个 key，每个 key 至少应该有 1 个 client
	for i := 0; i < 5; i++ {
		if counts[i] == 0 {
			t.Errorf("key %d got zero clients (distribution: %v)", i, counts)
		}
	}
	t.Logf("key distribution: %v", counts)
}

func TestHashClientKey_Deterministic(t *testing.T) {
	h1 := hashClientKey("alice", "target-a")
	h2 := hashClientKey("alice", "target-a")
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %d != %d", h1, h2)
	}

	// 不同 target 应该产生不同 hash（避免跨 target 碰撞）
	h3 := hashClientKey("alice", "target-b")
	if h1 == h3 {
		t.Errorf("different targets should produce different hashes")
	}
}

func TestIsKeyExhausted(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantExh    bool
		wantCode   string
	}{
		// ── 2xx 不算 ──
		{"200 OK", 200, `{"ok":true}`, false, ""},

		// ── 纯 429 限流不算（RPS/RPM 级别）──
		// 百炼: Throttling / Throttling.RateQuota / Throttling.BurstRate
		{"百炼 429 Throttling 纯限流", 429, `{"code":"Throttling","message":"Requests throttling triggered."}`, false, ""},
		{"百炼 429 Throttling.RateQuota", 429, `{"code":"Throttling.RateQuota","message":"You have exceeded your request limit."}`, false, ""},
		{"百炼 429 Throttling.BurstRate", 429, `{"code":"Throttling.BurstRate","message":"Request rate increased too quickly."}`, false, ""},
		// OpenAI: "Rate limit reached for requests"
		{"OpenAI 429 rate limit", 429, `{"error":{"message":"Rate limit reached for requests","type":"rate_limit_error"}}`, false, ""},
		// Claude: rate_limit_error
		{"Claude 429 rate_limit_error", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded."}}`, false, ""},
		// DeepSeek: "Rate Limit Reached"
		{"DeepSeek 429 rate limit", 429, `{"error":{"message":"Rate Limit Reached","type":"rate_limit_reached"}}`, false, ""},

		// ── 百炼 (alibabacloud.com/help/en/model-studio/error-code) ──
		// 401 InvalidApiKey
		{"百炼 401 InvalidApiKey", 401, `{"code":"InvalidApiKey","message":"Invalid API-key provided."}`, true, "invalid_token"},
		// 400 Arrearage
		{"百炼 400 Arrearage", 400, `{"code":"Arrearage","message":"Access denied, please make sure your account is in good standing."}`, true, "Arrearage"},
		// 403 AccessDenied.Unpurchased
		{"百炼 403 AccessDenied.Unpurchased", 403, `{"code":"AccessDenied.Unpurchased","message":"Access to model denied. Please make sure you are eligible for using the model."}`, true, "AccessDenied.Unpurchased"},
		// 403 AllocationQuota.FreeTierOnly
		{"百炼 403 FreeTierOnly", 403, `{"code":"AllocationQuota.FreeTierOnly","message":"The free tier of the model has been exhausted."}`, true, "free_tier_exhausted"},
		// 429 Throttling.AllocationQuota — TPS/TPM 配额耗尽（不是纯限流！）
		{"百炼 429 Throttling.AllocationQuota", 429, `{"code":"Throttling.AllocationQuota","message":"Allocated quota exceeded, please increase your quota limit."}`, true, "quota_exceeded"},
		// 429 PrepaidBillOverdue
		{"百炼 429 PrepaidBillOverdue", 429, `{"code":"PrepaidBillOverdue","message":"The prepaid bill is overdue."}`, true, "quota_exceeded"},
		// 429 PostpaidBillOverdue
		{"百炼 429 PostpaidBillOverdue", 429, `{"code":"PostpaidBillOverdue","message":"The postpaid bill is overdue."}`, true, "quota_exceeded"},
		// 429 CommodityNotPurchased
		{"百炼 429 CommodityNotPurchased", 429, `{"code":"CommodityNotPurchased","message":"Commodity has not purchased yet."}`, true, "quota_exceeded"},
		// 429 TokenPlan 配额耗尽（code=Throttling 但 message 含 "upgrade your API plan"）
		{"百炼 429 TokenPlan quota exhausted", 429, "event:error\ndata:{\"request_id\":\"abc\",\"code\":\"Throttling\",\"message\":\"Rate limit exceeded. Please wait and try again, or upgrade your API plan.\"}", true, "quota_exceeded"},

		// ── OpenAI (developers.openai.com/api/docs/guides/error-codes) ──
		// 401 "Incorrect API key provided"
		{"OpenAI 401 incorrect key", 401, `{"error":{"message":"Incorrect API key provided: sk-***...***","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`, true, "invalid_token"},
		// 429 "You exceeded your current quota"
		{"OpenAI 429 quota exceeded", 429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, true, "quota_exceeded"},

		// ── Azure OpenAI ──
		// 401 "Access denied due to invalid subscription key"
		{"Azure 401 invalid subscription key", 401, `{"error":{"code":"401","message":"Access denied due to invalid subscription key or wrong API endpoint."}}`, true, "invalid_token"},

		// ── Claude (platform.claude.com/docs/en/api/errors) ──
		// 401 authentication_error: "invalid x-api-key"
		{"Claude 401 authentication_error", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, true, "invalid_token"},
		// 402 billing_error
		{"Claude 402 billing_error", 402, `{"type":"error","error":{"type":"billing_error","message":"Your credit balance is too low to access the Anthropic API."}}`, true, "billing_error"},

		// ── Gemini (ai.google.dev/gemini-api/docs/troubleshooting) ──
		// 400 INVALID_ARGUMENT: "API key not valid"
		{"Gemini 400 API_KEY_INVALID", 400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`, true, "invalid_token"},
		// 429 RESOURCE_EXHAUSTED
		{"Gemini 429 RESOURCE_EXHAUSTED", 429, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`, true, "quota_exceeded"},

		// ── DeepSeek (api-docs.deepseek.com/quick_start/error_codes) ──
		// 401 Authentication Fails
		{"DeepSeek 401 auth fails", 401, `{"error":{"message":"Authentication Fails, Your api key: **** is invalid","type":"authentication_error","param":"","code":"invalid_api_key"}}`, true, "invalid_token"},
		// 402 Insufficient Balance
		{"DeepSeek 402 insufficient balance", 402, `{"error":{"message":"Insufficient Balance","type":"insufficient_balance","param":"","code":"insufficient_balance"}}`, true, "billing_error"},

		// ── 不匹配的错误 ──
		{"500 internal error", 500, `{"error":"internal server error"}`, false, ""},
		{"403 forbidden", 403, `{"error":"forbidden"}`, false, ""},
		{"503 overloaded", 503, `{"error":"The engine is currently overloaded, please try again later"}`, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotExh, gotCode := isKeyExhausted(tt.statusCode, []byte(tt.body))
			if gotExh != tt.wantExh {
				t.Errorf("isKeyExhausted() exhausted = %v, want %v (body: %s)", gotExh, tt.wantExh, tt.body)
			}
			if gotCode != tt.wantCode {
				t.Errorf("isKeyExhausted() code = %q, want %q", gotCode, tt.wantCode)
			}
		})
	}
}
