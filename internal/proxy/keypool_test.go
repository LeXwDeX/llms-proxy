package proxy

import (
	"fmt"
	"log/slog"
	"testing"
	"time"
)

func newTestKeyPool(keys []string) *keyPool {
	return newKeyPool("test-target", keys, "", slog.Default())
}

func TestSelectKeyForClient_Affinity(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b", "key-c", "key-d"})

	// 同一 client 首次分配后应该始终返回同一个 key（亲和保持）
	key1, idx1 := pool.selectKeyForClient("alice")
	key2, idx2 := pool.selectKeyForClient("alice")
	if key1 != key2 || idx1 != idx2 {
		t.Errorf("same client should get same key after affinity: got (%s,%d) and (%s,%d)", key1, idx1, key2, idx2)
	}

	// 多次调用仍然保持亲和
	for i := 0; i < 5; i++ {
		k, idx := pool.selectKeyForClient("alice")
		if k != key1 || idx != idx1 {
			t.Errorf("iteration %d: affinity broken, got (%s,%d) want (%s,%d)", i, k, idx, key1, idx1)
		}
	}
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

func TestSelectKeyForClient_RoundRobin(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2", "key-3", "key-4"})

	// 5 个不同客户端应该按轮询分配到 5 个不同的 key
	assigned := make(map[string]int)
	for i := 0; i < 5; i++ {
		client := fmt.Sprintf("client-%d", i)
		_, idx := pool.selectKeyForClient(client)
		assigned[client] = idx
	}

	// 验证每个 key 恰好被分配一次
	keyCount := make(map[int]int)
	for _, idx := range assigned {
		keyCount[idx]++
	}
	for i := 0; i < 5; i++ {
		if keyCount[i] != 1 {
			t.Errorf("key %d assigned %d times, want 1 (assignments: %v)", i, keyCount[i], assigned)
		}
	}
}

func TestSelectKeyForClient_AffinityAfterRoundRobin(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 3 个客户端轮询分配
	_, idx0 := pool.selectKeyForClient("c0")
	_, idx1 := pool.selectKeyForClient("c1")
	_, idx2 := pool.selectKeyForClient("c2")

	// 再次请求应该返回之前分配的 key（亲和保持）
	_, got0 := pool.selectKeyForClient("c0")
	_, got1 := pool.selectKeyForClient("c1")
	_, got2 := pool.selectKeyForClient("c2")

	if got0 != idx0 || got1 != idx1 || got2 != idx2 {
		t.Errorf("affinity broken after round-robin: want (%d,%d,%d) got (%d,%d,%d)",
			idx0, idx1, idx2, got0, got1, got2)
	}
}

func TestSelectKeyForClient_Distribution(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2", "key-3", "key-4"})

	clients := []string{
		"client-a", "client-b", "client-c", "client-d", "client-e",
		"client-f", "client-g", "client-h", "client-i", "client-j",
	}

	distribution := make(map[int]int)
	for _, client := range clients {
		_, idx := pool.selectKeyForClient(client)
		distribution[idx]++
	}

	// 验证分布：每个 key 至少被选中一次
	for i := 0; i < 5; i++ {
		if distribution[i] == 0 {
			t.Errorf("key %d was never selected, distribution: %v", i, distribution)
		}
	}
}

func TestMarkExhausted_RateLimited(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b"})
	pool.markExhausted(0, "rate_limited")

	// rate_limited 应该 60 秒冷却
	if pool.entries[0].cooldownEnd.Sub(pool.entries[0].exhaustedAt) != 60*time.Second {
		t.Errorf("rate_limited should have 60s cooldown, got %v", pool.entries[0].cooldownEnd.Sub(pool.entries[0].exhaustedAt))
	}
}

func TestMarkExhausted_QuotaExceeded_WithResetTime(t *testing.T) {
	// 设置一个未来的 resetTime
	cst := time.FixedZone("CST", 8*3600)
	futureReset := time.Now().In(cst).AddDate(0, 0, 7) // 7天后
	resetStr := futureReset.Format("2006-01-02 15:04")

	pool := newKeyPool("test", []string{"key-a"}, resetStr, slog.Default())
	pool.markExhausted(0, "quota_exceeded")

	// cooldownEnd 应该等于 resetTime
	if !pool.entries[0].cooldownEnd.Equal(pool.resetTime) {
		t.Errorf("quota_exceeded with future resetTime should use resetTime, got %v want %v", pool.entries[0].cooldownEnd, pool.resetTime)
	}
}

func TestMarkExhausted_QuotaExceeded_NoResetTime(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a"})
	pool.markExhausted(0, "quota_exceeded")

	// 没有 resetTime，应该永久屏蔽（cooldownEnd 在远未来）
	if pool.entries[0].cooldownEnd.Year() < 9000 {
		t.Errorf("quota_exceeded without resetTime should be permanent, got %v", pool.entries[0].cooldownEnd)
	}
}

func TestSelectKey_CooldownEnd(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a", "key-b"})
	pool.markExhausted(0, "rate_limited")
	pool.markExhausted(1, "rate_limited")

	// 全部在冷却中
	key, idx := pool.selectKey()
	if key != "" || idx != -1 {
		t.Errorf("all in cooldown should return empty, got (%s, %d)", key, idx)
	}

	// 手动设置 cooldownEnd 为过去
	pool.entries[0].cooldownEnd = time.Now().Add(-1 * time.Second)

	key, idx = pool.selectKey()
	if key != "key-a" || idx != 0 {
		t.Errorf("should recover key-a after cooldown, got (%s, %d)", key, idx)
	}
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

		// ── 纯 429 限流（RPS/RPM 级别，现在算 rate_limited，60秒冷却）──
		// 百炼: Throttling / Throttling.RateQuota / Throttling.BurstRate
		{"百炼 429 Throttling 纯限流", 429, `{"code":"Throttling","message":"Requests throttling triggered."}`, true, "rate_limited"},
		{"百炼 429 Throttling.RateQuota", 429, `{"code":"Throttling.RateQuota","message":"You have exceeded your request limit."}`, true, "rate_limited"},
		{"百炼 429 Throttling.BurstRate", 429, `{"code":"Throttling.BurstRate","message":"Request rate increased too quickly."}`, true, "rate_limited"},
		// OpenAI: "Rate limit reached for requests"
		{"OpenAI 429 rate limit", 429, `{"error":{"message":"Rate limit reached for requests","type":"rate_limit_error"}}`, true, "rate_limited"},
		// Claude: rate_limit_error
		{"Claude 429 rate_limit_error", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit exceeded."}}`, true, "rate_limited"},
		// DeepSeek: "Rate Limit Reached"
		{"DeepSeek 429 rate limit", 429, `{"error":{"message":"Rate Limit Reached","type":"rate_limit_reached"}}`, true, "rate_limited"},

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
		// 429 TokenPlan 额度耗尽（OpenAI 兼容模式，明文 JSON）
		{"百炼 429 TokenPlan quota has been exhausted", 429, `{"error":{"message":"Your token-plan quota has been exhausted.","id":"abc","type":"insufficient_quota","code":"insufficient_quota"}}`, true, "quota_exceeded"},

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
