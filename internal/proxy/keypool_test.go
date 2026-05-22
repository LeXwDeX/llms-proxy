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
