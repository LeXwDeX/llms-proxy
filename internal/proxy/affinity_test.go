package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
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

// TestAffinityFailoverDoesNotHijack 验证 failover 场景不劫持粘连：
// 当粘连目标暂时不可用（muted），请求 failover 到其他目标后，
// 粘连记录仍指向原目标；原目标恢复后流量自动回来。
func TestAffinityFailoverDoesNotHijack(t *testing.T) {
	// 两个 mock 上游，都返回 200
	targetA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"a"}`))
	}))
	defer targetA.Close()

	targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"b"}`))
	}))
	defer targetB.Close()

	aURL, _ := url.Parse(targetA.URL)
	bURL, _ := url.Parse(targetB.URL)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-a",
				Endpoint:           targetA.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-a",
				ModelMappings: []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
			{
				Name:               "target-b",
				Endpoint:           targetB.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-b",
				ModelMappings: []config.ModelMapping{{Upstream: "qwen3.7-max"}},
			},
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.quietPeriod = 50 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected authenticated principal")
	}

	sendRequest := func() string {
		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		res := rr.Result()
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", res.StatusCode)
		}
		return res.Header.Get("X-Proxy-Target")
	}

	// 步骤 1：手动建立粘连到 target-a
	service.affinity.Set(affinityKey("192.0.2.1", "tester", "qwen3.7-max"), "target-a", time.Now())

	// 步骤 2：验证粘连命中 target-a
	got := sendRequest()
	if got != "target-a" {
		t.Fatalf("step 2: expected affinity hit target-a, got %q", got)
	}

	// 步骤 3：mute target-a，模拟一次失败
	now := time.Now()
	stateA, exists := service.targetByName("target-a")
	if !exists {
		t.Fatal("target-a not found")
	}
	stateA.MarkFailure(now, 200*time.Millisecond)

	// 步骤 4：发送请求 → failover 到 target-b
	got = sendRequest()
	if got != "target-b" {
		t.Fatalf("step 4: expected failover to target-b, got %q", got)
	}

	// 步骤 5：验证粘连仍然指向 target-a（未被劫持到 target-b）
	aKey := affinityKey("192.0.2.1", "tester", "qwen3.7-max")
	affinityTarget, affOK := service.affinity.Get(aKey, time.Now())
	if !affOK {
		t.Fatal("step 5: expected affinity record to still exist")
	}
	if affinityTarget != "target-a" {
		t.Fatalf("step 5: expected affinity still pointing to target-a, got %q", affinityTarget)
	}

	// 步骤 6：等待 mute 过期，target-a 恢复
	time.Sleep(250 * time.Millisecond)

	// 步骤 7：发送请求 → 粘连命中 target-a，流量回来
	got = sendRequest()
	if got != "target-a" {
		t.Fatalf("step 7: expected affinity hit target-a after recovery, got %q", got)
	}

	_ = aURL
	_ = bURL
}

// TestAffinityKeyUsesUpstreamModelWhenAliasRequested 验证粘连 key 使用上游模型名
// 而非客户端 alias：当目标配置了 ModelMappings fallback（alias→upstream），
// 客户端用 alias 请求时，set/get 都应使用上游模型名作为 key，让不同 alias 但指向
// 同一上游的请求命中同一条粘连记录。
func TestAffinityKeyUsesUpstreamModelWhenAliasRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"ok"}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Bind: "127.0.0.1:0", RequestTimeoutSeconds: 5},
		Targets: []config.Target{{
			Name:          "t1",
			Endpoint:      srv.URL,
			APIKey:        "key",
			ModelMappings: []config.ModelMapping{{Upstream: "Real-Model-Name", Fallback: "shortcut"}},
		}},
		Logging: config.LoggingConfig{Level: "info", AccessLog: "logs/test-access.log", ErrorLog: "logs/test-error.log"},
	}
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// 客户端用 alias "shortcut" 请求
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"shortcut","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	now := time.Now()
	// 断言 1：粘连 key 应该使用上游名（保留原始大小写）。
	upKey := affinityKey("192.0.2.1", "tester", "Real-Model-Name")
	_, upOK := service.affinity.Get(upKey, now)
	if !upOK {
		t.Error("expected affinity entry under upstream model name key, but not found")
	}

	// 断言 2：粘连 key 不应该在 alias 名下（避免 Set 用 upstream、Get 用 alias 导致的 key 不匹配）。
	aliasKey := affinityKey("192.0.2.1", "tester", "shortcut")
	_, aliasOK := service.affinity.Get(aliasKey, now)
	if aliasOK {
		t.Error("affinity entry unexpectedly stored under alias key — Set should use upstream model name")
	}
}
