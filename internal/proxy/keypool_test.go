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

	// 10 个客户端分配到 5 个 key，每个 key 应该被分配 2 次
	assigned := make(map[string]int)
	for _, c := range clients {
		_, idx := pool.selectKeyForClient(c)
		assigned[c] = idx
	}

	keyCount := make(map[int]int)
	for _, idx := range assigned {
		keyCount[idx]++
	}
	for i := 0; i < 5; i++ {
		if keyCount[i] != 2 {
			t.Errorf("key %d assigned %d times, want 2 (assignments: %v)", i, keyCount[i], assigned)
		}
	}
}

// TestSelectKeyForClient_TokenAware 验证 token 感知调度：新客户端优先分配到累计 token 最少的 key。
func TestSelectKeyForClient_TokenAware(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 模拟 key-0 已经消耗了大量 token
	pool.recordTokens(0, 10000)
	pool.recordTokens(0, 5000)

	// 模拟 key-1 消耗了少量 token
	pool.recordTokens(1, 1000)

	// key-2 没有消耗（0 token）

	// 新客户端应该分配到 key-2（累计最少）
	_, idx := pool.selectKeyForClient("new-client")
	if idx != 2 {
		t.Errorf("expected new client assigned to key-2 (least tokens), got key-%d", idx)
	}

	// 再分配一个客户端，应该还是 key-2（因为 key-2 现在也只有刚分配的请求的 token，但还没 record）
	_, idx2 := pool.selectKeyForClient("new-client-2")
	if idx2 != 2 {
		t.Errorf("expected second new client assigned to key-2, got key-%d", idx2)
	}

	// 给 key-2 记录大量 token
	pool.recordTokens(2, 20000)

	// 下一个新客户端应该分配到 key-1（现在最少）
	_, idx3 := pool.selectKeyForClient("new-client-3")
	if idx3 != 1 {
		t.Errorf("expected third new client assigned to key-1 (now least tokens), got key-%d", idx3)
	}
}

// TestSelectKeyForClient_TokenAwareFallbackToRoundRobin 验证无 token 数据时退化为轮询。
func TestSelectKeyForClient_TokenAwareFallbackToRoundRobin(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 没有任何 token 数据，应该退化为轮询
	assigned := make(map[int]int)
	for i := 0; i < 6; i++ {
		client := fmt.Sprintf("client-%d", i)
		_, idx := pool.selectKeyForClient(client)
		assigned[idx]++
	}

	// 轮询应该均匀分配
	for i := 0; i < 3; i++ {
		if assigned[i] != 2 {
			t.Errorf("key-%d assigned %d times, want 2 (round-robin fallback)", i, assigned[i])
		}
	}
}

// TestRecordTokens_SlidingWindow 验证滑动窗口过期清理。
func TestRecordTokens_SlidingWindow(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})
	pool.tokenWindow = 100 * time.Millisecond // 缩短窗口便于测试

	// 记录 token
	pool.recordTokens(0, 1000)
	pool.recordTokens(0, 2000)

	// 验证累计
	sum := pool.tokenSumInWindow(0, time.Now())
	if sum != 3000 {
		t.Errorf("expected sum 3000, got %d", sum)
	}

	// 等待窗口过期
	time.Sleep(150 * time.Millisecond)

	// 过期后应该为 0
	sum = pool.tokenSumInWindow(0, time.Now())
	if sum != 0 {
		t.Errorf("expected sum 0 after window expired, got %d", sum)
	}

	// 记录新的 token
	pool.recordTokens(0, 500)
	sum = pool.tokenSumInWindow(0, time.Now())
	if sum != 500 {
		t.Errorf("expected sum 500 after new record, got %d", sum)
	}
}

// TestLeastTokenActiveKeyIndex 验证 least-token 选择逻辑。
func TestLeastTokenActiveKeyIndex(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 无数据时返回 -2（退化为轮询）
	idx := pool.leastTokenActiveKeyIndex()
	if idx != -2 {
		t.Errorf("expected -2 (no data, fallback to round-robin), got %d", idx)
	}

	// 记录不同 token 量
	pool.recordTokens(0, 5000)
	pool.recordTokens(1, 1000)
	pool.recordTokens(2, 3000)

	// 应该返回 key-1（最少）
	idx = pool.leastTokenActiveKeyIndex()
	if idx != 1 {
		t.Errorf("expected key-1 (least tokens), got key-%d", idx)
	}

	// 标记 key-1 为 exhausted
	pool.markExhausted(1, "quota_exceeded")

	// 应该返回 key-2（现在最少）
	idx = pool.leastTokenActiveKeyIndex()
	if idx != 2 {
		t.Errorf("expected key-2 (least among active), got key-%d", idx)
	}

	// 全部 exhausted
	pool.markExhausted(0, "quota_exceeded")
	pool.markExhausted(2, "quota_exceeded")

	// 应该返回 -1（无 active key）
	idx = pool.leastTokenActiveKeyIndex()
	if idx != -1 {
		t.Errorf("expected -1 (no active key), got %d", idx)
	}
}

// TestMarkRateLimited 验证限流标记和冷却恢复。
func TestMarkRateLimited(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 标记 key-0 为限流
	pool.markRateLimited(0)

	// key-0 应该被标记为 exhausted
	if !pool.entries[0].exhausted {
		t.Error("expected key-0 to be marked exhausted")
	}
	if pool.entries[0].exhaustReason != "rate_limited" {
		t.Errorf("expected exhaustReason 'rate_limited', got %q", pool.entries[0].exhaustReason)
	}

	// 冷却时间应该是 5 秒后
	expectedCooldown := time.Now().Add(5 * time.Second)
	diff := pool.entries[0].cooldownEnd.Sub(expectedCooldown)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected cooldown around 5s, got %v", pool.entries[0].cooldownEnd.Sub(time.Now()))
	}

	// selectKeyForClient 应该返回 key-1（key-0 被限流）
	_, idx := pool.selectKeyForClient("alice")
	if idx != 1 {
		t.Errorf("expected key-1 (key-0 rate limited), got key-%d", idx)
	}

	// 等待冷却过期
	time.Sleep(50 * time.Millisecond)
	// 手动设置冷却时间为过去（加速测试）
	pool.mu.Lock()
	pool.entries[0].cooldownEnd = time.Now().Add(-time.Second)
	pool.mu.Unlock()

	// 冷却过期后应该能恢复
	key, idx := pool.selectKey()
	if key == "" || idx == -1 {
		t.Error("expected key to recover after cooldown")
	}
}

// TestSelectNextActiveKey 验证切换到下一个 active key。
func TestSelectNextActiveKey(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 从 key-0 之后找，应该返回 key-1
	_, idx := pool.selectNextActiveKey(0)
	if idx != 1 {
		t.Errorf("expected key-1 after key-0, got key-%d", idx)
	}

	// 标记 key-1 为 exhausted
	pool.markExhausted(1, "quota_exceeded")

	// 从 key-0 之后找，应该跳过 key-1 返回 key-2
	_, idx = pool.selectNextActiveKey(0)
	if idx != 2 {
		t.Errorf("expected key-2 (skip exhausted key-1), got key-%d", idx)
	}

	// 标记 key-2 也为 exhausted
	pool.markExhausted(2, "quota_exceeded")

	// 从 key-0 之后找，应该绕回 key-0
	_, idx = pool.selectNextActiveKey(0)
	if idx != 0 {
		t.Errorf("expected key-0 (wrap around), got key-%d", idx)
	}

	// 全部 exhausted
	pool.markExhausted(0, "quota_exceeded")

	// 应该返回 -1（无 active key）
	_, idx = pool.selectNextActiveKey(0)
	if idx != -1 {
		t.Errorf("expected -1 (no active key), got %d", idx)
	}
}

// TestPeriodicResetTime 验证周期性 resetTime（monthly:23）。
func TestPeriodicResetTime(t *testing.T) {
	// 测试 monthly:23 格式
	pool := newKeyPool("test", []string{"key-a"}, "monthly:23", slog.Default())
	if pool.resetTime.IsZero() {
		t.Error("expected resetTime to be set for monthly:23")
	}
	// 验证日期是 23 号
	if pool.resetTime.Day() != 23 {
		t.Errorf("expected day 23, got %d", pool.resetTime.Day())
	}

	// 测试无效格式
	pool2 := newKeyPool("test", []string{"key-a"}, "monthly:32", slog.Default())
	if !pool2.resetTime.IsZero() {
		t.Error("expected resetTime to be zero for invalid monthly:32")
	}

	pool3 := newKeyPool("test", []string{"key-a"}, "monthly:abc", slog.Default())
	if !pool3.resetTime.IsZero() {
		t.Error("expected resetTime to be zero for invalid monthly:abc")
	}
}

// TestDesperationProbe 验证绝境探测机制。
func TestDesperationProbe(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 标记所有 key 为 exhausted，但冷却时间不同
	now := time.Now()
	pool.mu.Lock()
	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "quota_exceeded"
	pool.entries[0].cooldownEnd = now.Add(5 * time.Minute) // 5 分钟后

	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "quota_exceeded"
	pool.entries[1].cooldownEnd = now.Add(1 * time.Minute) // 1 分钟后（最短）

	pool.entries[2].exhausted = true
	pool.entries[2].exhaustReason = "quota_exceeded"
	pool.entries[2].cooldownEnd = now.Add(10 * time.Minute) // 10 分钟后
	pool.mu.Unlock()

	// selectKey 应该返回冷却最短的 key-1 做绝境探测
	_, idx := pool.selectKey()
	if idx != 1 {
		t.Errorf("expected desperation probe to select key-1 (shortest cooldown), got key-%d", idx)
	}

	// 模拟绝境探测成功，标记 key-1 为已恢复
	pool.markRecovered(1)

	// key-1 应该恢复为 active
	if pool.entries[1].exhausted {
		t.Error("expected key-1 to be recovered after markRecovered")
	}

	// 现在 selectKey 应该返回 key-1（唯一的 active key）
	_, idx = pool.selectKey()
	if idx != 1 {
		t.Errorf("expected key-1 after recovery, got key-%d", idx)
	}
}

// TestDesperationProbe_SkipRateLimited 验证绝境探测跳过 rate_limited key。
func TestDesperationProbe_SkipRateLimited(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	now := time.Now()
	pool.mu.Lock()
	// key-0 是 rate_limited（应该被跳过）
	pool.entries[0].exhausted = true
	pool.entries[0].exhaustReason = "rate_limited"
	pool.entries[0].cooldownEnd = now.Add(30 * time.Second)

	// key-1 是 quota_exceeded
	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "quota_exceeded"
	pool.entries[1].cooldownEnd = now.Add(5 * time.Minute)
	pool.mu.Unlock()

	// 绝境探测应该跳过 rate_limited 的 key-0，选 key-1
	_, idx := pool.selectKey()
	if idx != 1 {
		t.Errorf("expected desperation probe to skip rate_limited key-0 and select key-1, got key-%d", idx)
	}
}

// TestMarkRecovered 验证 markRecovered 方法。
func TestMarkRecovered(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	// 标记为 exhausted
	pool.markExhausted(0, "quota_exceeded")
	if !pool.entries[0].exhausted {
		t.Error("expected key-0 to be exhausted")
	}

	// 标记为已恢复
	if !pool.markRecovered(0) {
		t.Error("expected markRecovered to return true when recovering an exhausted key")
	}
	if pool.entries[0].exhausted {
		t.Error("expected key-0 to be recovered")
	}
	if pool.entries[0].exhaustReason != "" {
		t.Errorf("expected exhaustReason to be cleared, got %q", pool.entries[0].exhaustReason)
	}

	// 对已经 active 的 key 调用 markRecovered 应该是 no-op 且返回 false
	if pool.markRecovered(0) {
		t.Error("expected markRecovered to return false for an already-active key")
	}
	if pool.entries[0].exhausted {
		t.Error("expected key-0 to remain active")
	}

	// 对仅手动 blocked（未 exhausted）的 key 调用应返回 false，且不清除 blocked 标记
	pool.blockKey(0)
	if pool.markRecovered(0) {
		t.Error("expected markRecovered to return false for a manually-blocked key")
	}
	if !pool.entries[0].blocked {
		t.Error("expected manual block to be preserved after markRecovered")
	}
}

func TestBlockKey(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 屏蔽 key-0
	pool.blockKey(0)
	if !pool.entries[0].blocked {
		t.Error("expected key-0 to be blocked")
	}

	// selectKeyForClient 应该跳过 blocked key
	key, idx := pool.selectKeyForClient("client1")
	if idx == 0 {
		t.Error("expected to skip blocked key-0")
	}
	if key != "key-1" {
		t.Errorf("expected key-1, got %s", key)
	}

	// 边界条件：nil pool
	var nilPool *keyPool
	nilPool.blockKey(0) // should not panic

	// 边界条件：越界索引
	pool.blockKey(-1)
	pool.blockKey(100)
}

func TestUnblockKey(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	// 先屏蔽
	pool.blockKey(0)
	if !pool.entries[0].blocked {
		t.Error("expected key-0 to be blocked")
	}

	// 解除屏蔽
	pool.unblockKey(0)
	if pool.entries[0].blocked {
		t.Error("expected key-0 to be unblocked")
	}
	if pool.entries[0].exhausted {
		t.Error("expected exhausted to be cleared")
	}

	// 边界条件：nil pool
	var nilPool *keyPool
	nilPool.unblockKey(0) // should not panic

	// 边界条件：越界索引
	pool.unblockKey(-1)
	pool.unblockKey(100)
}

func TestKeyPoolStatus(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 初始状态
	statuses := pool.status()
	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(statuses))
	}

	// 第一个 key 应该是"使用中"
	if statuses[0].Status != "使用中" {
		t.Errorf("expected first key status to be '使用中', got %q", statuses[0].Status)
	}

	// 其他 key 应该是"等待中"
	if statuses[1].Status != "等待中" {
		t.Errorf("expected second key status to be '等待中', got %q", statuses[1].Status)
	}

	// 屏蔽一个 key
	pool.blockKey(1)
	statuses = pool.status()
	if statuses[1].Status != "已屏蔽" {
		t.Errorf("expected blocked key status to be '已屏蔽', got %q", statuses[1].Status)
	}

	// 标记一个 key 为额度超限（无 resetTime → quota_exceeded_api → 人工恢复）
	pool.markExhausted(2, "quota_exceeded")
	statuses = pool.status()
	if statuses[2].Status != "额度超限(人工恢复)" {
		t.Errorf("expected exhausted key status to be '额度超限(人工恢复)', got %q", statuses[2].Status)
	}

	// 标记一个 key 为限流中
	pool.markRateLimited(0)
	statuses = pool.status()
	if statuses[0].Status != "限流中" {
		t.Errorf("expected rate limited key status to be '限流中', got %q", statuses[0].Status)
	}

	// 边界条件：nil pool
	var nilPool *keyPool
	if nilPool.status() != nil {
		t.Error("expected nil pool to return nil status")
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
	// exhaustReason 应该是 quota_exceeded_subscription
	if pool.entries[0].exhaustReason != "quota_exceeded_subscription" {
		t.Errorf("expected exhaustReason 'quota_exceeded_subscription', got %q", pool.entries[0].exhaustReason)
	}
}

func TestMarkExhausted_QuotaExceeded_NoResetTime(t *testing.T) {
	pool := newTestKeyPool([]string{"key-a"})
	pool.markExhausted(0, "quota_exceeded")

	// 没有 resetTime，应该永久屏蔽（cooldownEnd 在远未来）
	if pool.entries[0].cooldownEnd.Year() < 9000 {
		t.Errorf("quota_exceeded without resetTime should be permanent, got %v", pool.entries[0].cooldownEnd)
	}
	// exhaustReason 应该是 quota_exceeded_api
	if pool.entries[0].exhaustReason != "quota_exceeded_api" {
		t.Errorf("expected exhaustReason 'quota_exceeded_api', got %q", pool.entries[0].exhaustReason)
	}
}

// TestQuotaExceeded_StatusDisplay 验证两种超额子类型的状态显示。
func TestQuotaExceeded_StatusDisplay(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	futureReset := time.Now().In(cst).AddDate(0, 0, 7)
	resetStr := futureReset.Format("2006-01-02 15:04")

	pool := newKeyPool("test", []string{"key-sub", "key-api"}, resetStr, slog.Default())

	// key-0: 有 resetTime → quota_exceeded_subscription → "额度超限(自动恢复)"
	pool.markExhausted(0, "quota_exceeded")
	// key-1: 手动设置为无 resetTime 的情况（模拟 API 额度耗尽）
	pool.entries[1].exhausted = true
	pool.entries[1].exhaustReason = "quota_exceeded_api"
	pool.entries[1].cooldownEnd = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

	statuses := pool.status()

	if statuses[0].Status != "额度超限(自动恢复)" {
		t.Errorf("expected key-0 status '额度超限(自动恢复)', got %q", statuses[0].Status)
	}
	if statuses[1].Status != "额度超限(人工恢复)" {
		t.Errorf("expected key-1 status '额度超限(人工恢复)', got %q", statuses[1].Status)
	}
}

func TestSelectKey_CooldownEnd(t *testing.T) {
	// 用 quota_exceeded + 近未来 resetTime 测试冷却恢复
	cst := time.FixedZone("CST", 8*3600)
	futureReset := time.Now().In(cst).Add(1 * time.Hour)
	resetStr := futureReset.Format("2006-01-02 15:04")

	pool := newKeyPool("test-target", []string{"key-a", "key-b"}, resetStr, slog.Default())
	pool.markExhausted(0, "quota_exceeded")
	pool.markExhausted(1, "quota_exceeded")

	// 全部在冷却中：绝境探测应该返回冷却最短的 key（而不是空）
	key, idx := pool.selectKey()
	if key == "" || idx == -1 {
		t.Errorf("desperation probe should return a key even when all in cooldown, got (%s, %d)", key, idx)
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

		// ── 429 限流：不标记 key 耗尽（临时限速，key 没问题）──
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
		// 429 Throttling.AllocationQuota — TPM 限流（不是真正配额耗尽，不标记 key）
		{"百炼 429 Throttling.AllocationQuota", 429, `{"code":"Throttling.AllocationQuota","message":"Allocated quota exceeded, please increase your quota limit."}`, false, ""},
		// 429 PrepaidBillOverdue
		{"百炼 429 PrepaidBillOverdue", 429, `{"code":"PrepaidBillOverdue","message":"The prepaid bill is overdue."}`, true, "quota_exceeded"},
		// 429 PostpaidBillOverdue
		{"百炼 429 PostpaidBillOverdue", 429, `{"code":"PostpaidBillOverdue","message":"The postpaid bill is overdue."}`, true, "quota_exceeded"},
		// 429 CommodityNotPurchased
		{"百炼 429 CommodityNotPurchased", 429, `{"code":"CommodityNotPurchased","message":"Commodity has not purchased yet."}`, true, "quota_exceeded"},
		// 429 TokenPlan TPM 限流（"upgrade your API plan" 是限流建议，不是真正额度耗尽）
		{"百炼 429 TokenPlan TPM rate limit", 429, "event:error\ndata:{\"request_id\":\"abc\",\"code\":\"Throttling\",\"message\":\"Rate limit exceeded. Please wait and try again, or upgrade your API plan.\"}", false, ""},
		// 429 TokenPlan 总额度耗尽（OpenAI 兼容模式，"token-plan quota has been exhausted"）
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

// TestInvalidToken_5MinuteCooldown 验证 invalid_token 冷却时间为5分钟（非永久屏蔽）。
func TestInvalidToken_5MinuteCooldown(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 标记 key-0 为 invalid_token
	pool.markExhausted(0, "invalid_token")

	// key-0 应该被标记为 exhausted
	if !pool.entries[0].exhausted {
		t.Error("expected key-0 to be marked exhausted")
	}
	if pool.entries[0].exhaustReason != "invalid_token" {
		t.Errorf("expected exhaustReason 'invalid_token', got %q", pool.entries[0].exhaustReason)
	}

	// 冷却时间应该是 5 分钟后（非永久）
	expectedCooldown := time.Now().Add(5 * time.Minute)
	diff := pool.entries[0].cooldownEnd.Sub(expectedCooldown)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected cooldown around 5 minutes, got %v", pool.entries[0].cooldownEnd.Sub(time.Now()))
	}

	// 冷却时间不应该是永久（9999年）
	permanentEnd := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if pool.entries[0].cooldownEnd.Equal(permanentEnd) {
		t.Error("invalid_token should not use permanent cooldown")
	}
}

// TestBillingError_5MinuteCooldown 验证 billing_error 冷却时间为5分钟。
func TestBillingError_5MinuteCooldown(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	pool.markExhausted(0, "billing_error")

	expectedCooldown := time.Now().Add(5 * time.Minute)
	diff := pool.entries[0].cooldownEnd.Sub(expectedCooldown)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected cooldown around 5 minutes, got %v", pool.entries[0].cooldownEnd.Sub(time.Now()))
	}
}

// TestWakeUp_Singleton 验证唤醒模型单例化（并发调用只执行一次）。
func TestWakeUp_Singleton(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1"})

	// 标记所有 key 为 exhausted
	pool.markExhausted(0, "invalid_token")
	pool.markExhausted(1, "invalid_token")

	now := time.Now()

	// 第一次调用应该成功
	if !pool.tryWakeUp(now) {
		t.Error("first tryWakeUp should succeed")
	}

	// 第二次调用应该失败（已有协程在执行）
	if pool.tryWakeUp(now) {
		t.Error("second tryWakeUp should fail (singleton)")
	}

	// 完成唤醒
	pool.wakeUpComplete()

	// 冷却期内再次调用应该失败
	if pool.tryWakeUp(now.Add(30 * time.Second)) {
		t.Error("tryWakeUp within cooldown should fail")
	}

	// 冷却期后再次调用应该成功
	if !pool.tryWakeUp(now.Add(2 * time.Minute)) {
		t.Error("tryWakeUp after cooldown should succeed")
	}
	pool.wakeUpComplete()
}

// TestWakeUp_Cooldown 验证唤醒冷却（1分钟内不重复触发）。
func TestWakeUp_Cooldown(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0"})

	now := time.Now()

	// 第一次调用应该成功
	if !pool.tryWakeUp(now) {
		t.Error("first tryWakeUp should succeed")
	}
	pool.wakeUpComplete()

	// 冷却期内（30秒后）应该失败
	if pool.tryWakeUp(now.Add(30 * time.Second)) {
		t.Error("tryWakeUp within 1 minute cooldown should fail")
	}

	// 冷却期后（2分钟后）应该成功
	if !pool.tryWakeUp(now.Add(2 * time.Minute)) {
		t.Error("tryWakeUp after 1 minute cooldown should succeed")
	}
	pool.wakeUpComplete()
}

// TestGetExhaustedKeys 验证获取被屏蔽的 key 列表（按冷却时间排序）。
func TestGetExhaustedKeys(t *testing.T) {
	pool := newTestKeyPool([]string{"key-0", "key-1", "key-2"})

	// 标记 key-0 为 invalid_token（5分钟冷却）
	pool.markExhausted(0, "invalid_token")
	// 标记 key-1 为 rate_limited（5秒冷却）
	pool.markRateLimited(1)
	// key-2 保持 active

	exhausted := pool.getExhaustedKeys()

	// 应该返回 2 个 key（key-0 和 key-1）
	if len(exhausted) != 2 {
		t.Errorf("expected 2 exhausted keys, got %d", len(exhausted))
	}

	// 应该按冷却时间排序：key-1（5秒）在前，key-0（5分钟）在后
	if exhausted[0] != 1 {
		t.Errorf("expected first exhausted key to be key-1 (shorter cooldown), got key-%d", exhausted[0])
	}
	if exhausted[1] != 0 {
		t.Errorf("expected second exhausted key to be key-0 (longer cooldown), got key-%d", exhausted[1])
	}
}
