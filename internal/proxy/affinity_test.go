package proxy

import (
	"testing"
	"time"
)

func TestAffinityMap(t *testing.T) {
	am := newAffinityMap()
	now := time.Now()

	// 空查询
	_, ok := am.Get("client1:gpt-4o", now)
	if ok {
		t.Fatal("expected miss on empty map")
	}

	// 写入+命中
	am.Set("client1:gpt-4o", "target-a", now)
	name, ok := am.Get("client1:gpt-4o", now)
	if !ok || name != "target-a" {
		t.Fatalf("expected hit target-a, got %q ok=%v", name, ok)
	}

	// 过期
	_, ok = am.Get("client1:gpt-4o", now.Add(affinityTTL+time.Second))
	if ok {
		t.Fatal("expected miss after TTL")
	}

	// 更新
	am.Set("client1:gpt-4o", "target-b", now.Add(time.Minute))
	name, ok = am.Get("client1:gpt-4o", now.Add(time.Minute))
	if !ok || name != "target-b" {
		t.Fatalf("expected hit target-b after update, got %q ok=%v", name, ok)
	}

	// 不同 key 隔离
	_, ok = am.Get("client2:gpt-4o", now)
	if ok {
		t.Fatal("expected miss for different client")
	}
}
