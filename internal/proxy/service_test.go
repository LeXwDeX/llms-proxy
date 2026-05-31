package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/copilot"
	"github.com/ycgame/llms-proxy/internal/nosql"
	"github.com/ycgame/llms-proxy/internal/usage"
)

type failingTransport struct {
	successHost string
}

func (t *failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "primary.local" {
		return nil, fmt.Errorf("dial error")
	}
	if strings.Contains(req.URL.Host, t.successHost) {
		return http.DefaultTransport.RoundTrip(req)
	}
	return nil, fmt.Errorf("unexpected host %q", req.URL.Host)
}

func TestServiceRoundRobinLoadBalancing(t *testing.T) {
	// 创建两个 mock 上游，都支持同一个模型
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target1.Close()

	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-1",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-1",
				AllowedModels:      []string{"qwen3.7-max"},
			},
			{
				Name:               "target-2",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-2",
				AllowedModels:      []string{"qwen3.7-max"},
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

	// 创建多个不同的客户端
	store := auth.NewStore()
	clients := []config.Client{
		{Name: "client-1", AccessKey: "token-1"},
		{Name: "client-2", AccessKey: "token-2"},
		{Name: "client-3", AccessKey: "token-3"},
		{Name: "client-4", AccessKey: "token-4"},
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 每个客户端发送一次请求，统计 target 分配
	targetCount := make(map[string]int)
	for _, client := range clients {
		principal, ok := store.Authenticate(client.AccessKey)
		if !ok {
			t.Fatalf("failed to authenticate client %s", client.Name)
		}

		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("client %s: expected 200, got %d", client.Name, rr.Code)
		}

		target := rr.Header().Get("X-Proxy-Target")
		if target == "" {
			t.Fatalf("client %s: missing X-Proxy-Target header", client.Name)
		}
		targetCount[target]++
	}

	// 验证：4 个客户端应该分配到 2 个 target，每个 target 至少 1 次
	if len(targetCount) < 2 {
		t.Errorf("expected requests distributed to 2 targets, got %d: %v", len(targetCount), targetCount)
	}

	// 验证：分配应该相对均匀（允许 3:1 或 2:2）
	for target, count := range targetCount {
		if count == 0 {
			t.Errorf("target %s received 0 requests", target)
		}
	}

	t.Logf("Target distribution: %v", targetCount)
}

func TestServiceLeastTokenLoadBalancing(t *testing.T) {
	// 创建两个 mock 上游，返回不同的 token 用量
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// target-1 返回大量 token
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10000,"completion_tokens":5000}}`))
	}))
	defer target1.Close()

	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// target-2 返回少量 token
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-heavy",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-1",
				AllowedModels:      []string{"qwen3.7-max"},
			},
			{
				Name:               "target-light",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-2",
				AllowedModels:      []string{"qwen3.7-max"},
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

	// 设置 mock usage recorder
	service.SetUsageRecorder(&mockUsageRecorder{})

	store := auth.NewStore()
	clients := []config.Client{
		{Name: "client-1", AccessKey: "token-1"},
		{Name: "client-2", AccessKey: "token-2"},
		{Name: "client-3", AccessKey: "token-3"},
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 第一个客户端发送请求（会随机分配到一个 target）
	principal1, _ := store.Authenticate("token-1")
	body1 := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body1)
	req1 = req1.WithContext(auth.WithPrincipal(req1.Context(), principal1))
	rr1 := httptest.NewRecorder()
	service.ServeHTTP(rr1, req1)
	firstTarget := rr1.Header().Get("X-Proxy-Target")

	// 第二个客户端发送请求（应该分配到 token 较少的 target）
	principal2, _ := store.Authenticate("token-2")
	body2 := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body2)
	req2 = req2.WithContext(auth.WithPrincipal(req2.Context(), principal2))
	rr2 := httptest.NewRecorder()
	service.ServeHTTP(rr2, req2)
	secondTarget := rr2.Header().Get("X-Proxy-Target")

	// 第三个客户端发送请求
	principal3, _ := store.Authenticate("token-3")
	body3 := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body3)
	req3 = req3.WithContext(auth.WithPrincipal(req3.Context(), principal3))
	rr3 := httptest.NewRecorder()
	service.ServeHTTP(rr3, req3)
	thirdTarget := rr3.Header().Get("X-Proxy-Target")

	t.Logf("First request: %s", firstTarget)
	t.Logf("Second request: %s", secondTarget)
	t.Logf("Third request: %s", thirdTarget)

	// 验证：至少有两个不同的 target 被使用
	targets := map[string]bool{firstTarget: true, secondTarget: true, thirdTarget: true}
	if len(targets) < 2 {
		t.Errorf("expected at least 2 different targets, got %d: %v", len(targets), targets)
	}
}

// mockUsageRecorder 是一个简单的 mock usage recorder
type mockUsageRecorder struct{}

func (m *mockUsageRecorder) Record(evt usage.Event) error {
	return nil
}

// TestServiceAffinityByIPAndKey 测试客户端亲和性按 IP+KEY 组合分配
// 场景：
// 1. 同一客户端（同 KEY）从不同 IP 访问，应该独立分配（不共享 affinity）
// 2. 不同客户端（不同 KEY）从同一 IP 访问，应该独立分配
// 3. 同一客户端从同一 IP 访问，应该命中 affinity（保持粘连）
func TestServiceAffinityByIPAndKey(t *testing.T) {
	// 创建两个 mock 上游
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target1.Close()

	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "target-1",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-1",
				AllowedModels:      []string{"qwen3.7-max"},
			},
			{
				Name:               "target-2",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key-2",
				AllowedModels:      []string{"qwen3.7-max"},
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

	store := auth.NewStore()
	clients := []config.Client{
		{Name: "client-A", AccessKey: "token-A"},
		{Name: "client-B", AccessKey: "token-B"},
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 辅助函数：发送请求并返回 target
	sendRequest := func(clientName, accessKey, remoteIP string) string {
		principal, ok := store.Authenticate(accessKey)
		if !ok {
			t.Fatalf("failed to authenticate client %s", clientName)
		}

		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req.RemoteAddr = remoteIP + ":12345" // 设置客户端 IP
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("client %s from %s: expected 200, got %d", clientName, remoteIP, rr.Code)
		}

		return rr.Header().Get("X-Proxy-Target")
	}

	// 场景 1：同一客户端从不同 IP 访问，应该独立分配（不共享 affinity）
	t.Run("SameKeyDifferentIP", func(t *testing.T) {
		// client-A 从 IP-1 访问
		target1 := sendRequest("client-A", "token-A", "192.168.1.1")
		// client-A 从 IP-2 访问（应该独立分配，不命中 IP-1 的 affinity）
		target2 := sendRequest("client-A", "token-A", "192.168.1.2")

		// 由于是首次访问，两个 IP 应该各自独立分配
		// 验证：至少有一个 target 被使用（可能是同一个，也可能是不同的）
		if target1 == "" || target2 == "" {
			t.Errorf("expected both targets to be non-empty, got target1=%s, target2=%s", target1, target2)
		}
		t.Logf("client-A from IP-1: %s, from IP-2: %s", target1, target2)
	})

	// 场景 2：不同客户端从同一 IP 访问，应该独立分配
	t.Run("DifferentKeySameIP", func(t *testing.T) {
		// client-A 从 IP-3 访问
		targetA := sendRequest("client-A", "token-A", "192.168.2.1")
		// client-B 从 IP-3 访问（应该独立分配，不命中 client-A 的 affinity）
		targetB := sendRequest("client-B", "token-B", "192.168.2.1")

		// 验证：至少有一个 target 被使用
		if targetA == "" || targetB == "" {
			t.Errorf("expected both targets to be non-empty, got targetA=%s, targetB=%s", targetA, targetB)
		}
		t.Logf("client-A from IP-3: %s, client-B from IP-3: %s", targetA, targetB)
	})

	// 场景 3：同一客户端从同一 IP 访问，应该命中 affinity（保持粘连）
	t.Run("SameKeySameIP", func(t *testing.T) {
		// client-A 从 IP-4 第一次访问
		target1 := sendRequest("client-A", "token-A", "192.168.3.1")
		// client-A 从 IP-4 第二次访问（应该命中 affinity，返回同一个 target）
		target2 := sendRequest("client-A", "token-A", "192.168.3.1")
		// client-A 从 IP-4 第三次访问（应该继续命中 affinity）
		target3 := sendRequest("client-A", "token-A", "192.168.3.1")

		// 验证：三次访问应该返回同一个 target（affinity 生效）
		if target1 != target2 || target2 != target3 {
			t.Errorf("expected affinity to keep same target, got target1=%s, target2=%s, target3=%s", target1, target2, target3)
		}
		t.Logf("client-A from IP-4 (3 times): %s, %s, %s", target1, target2, target3)
	})
}

func TestServiceModelAwareRoundRobin(t *testing.T) {
	// 场景：11 个 targets，只有 2 个支持 qwen3.7-max（索引 9 和 10）
	// target-9 有 4 个 key，target-10 有 1 个 key
	// 验证：按 key 数量轮询，target-9 获得 80% 流量，target-10 获得 20% 流量

	// 创建 11 个 mock 上游
	var servers []*httptest.Server
	for i := 0; i < 11; i++ {
		idx := i
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5},"target_index":%d}`, idx)))
		}))
		defer server.Close()
		servers = append(servers, server)
	}

	// 配置 11 个 targets，只有索引 9 和 10 支持 qwen3.7-max
	targets := make([]config.Target, 11)
	for i := 0; i < 11; i++ {
		targets[i] = config.Target{
			Name:               fmt.Sprintf("target-%d", i),
			Endpoint:           servers[i].URL,
			ResourcePathPrefix: "/",
			APIKey:             fmt.Sprintf("key-%d", i),
		}
		// 只有索引 9 和 10 支持 qwen3.7-max
		if i == 9 {
			// target-9 有 4 个 key（1 个 api_key + 3 个 api_keys）
			targets[i].AllowedModels = []string{"qwen3.7-max"}
			targets[i].APIKeys = []string{"key-9-2", "key-9-3", "key-9-4"}
		} else if i == 10 {
			// target-10 有 1 个 key（只有 api_key）
			targets[i].AllowedModels = []string{"qwen3.7-max"}
		} else {
			targets[i].AllowedModels = []string{"other-model"}
		}
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: targets,
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

	// 创建 20 个不同的客户端（避免 affinity 影响）
	store := auth.NewStore()
	clients := make([]config.Client, 20)
	for i := 0; i < 20; i++ {
		clients[i] = config.Client{
			Name:      fmt.Sprintf("client-%d", i),
			AccessKey: fmt.Sprintf("token-%d", i),
		}
	}
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}

	// 发送 20 个请求，统计 target 分布
	targetCount := make(map[string]int)
	for i := 0; i < 20; i++ {
		principal, ok := store.Authenticate(fmt.Sprintf("token-%d", i))
		if !ok {
			t.Fatalf("failed to authenticate client-%d", i)
		}

		body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req.RemoteAddr = fmt.Sprintf("192.168.%d.%d:12345", i/256, i%256) // 不同 IP
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("client-%d: expected 200, got %d", i, rr.Code)
		}

		target := rr.Header().Get("X-Proxy-Target")
		if target == "" {
			t.Fatalf("client-%d: missing X-Proxy-Target header", i)
		}
		targetCount[target]++
	}

	// 验证：只有 target-9 和 target-10 应该被选中
	if len(targetCount) != 2 {
		t.Errorf("expected exactly 2 targets to be selected, got %d: %v", len(targetCount), targetCount)
	}

	// 验证：target-9 应该获得约 80% 流量（16 个请求），target-10 应该获得约 20% 流量（4 个请求）
	// 允许 10% 偏差：target-9 应该 14-18 个，target-10 应该 2-6 个
	target9Count := targetCount["target-9"]
	target10Count := targetCount["target-10"]

	if target9Count < 14 || target9Count > 18 {
		t.Errorf("target-9 should get ~80%% traffic (14-18 requests), got %d", target9Count)
	}
	if target10Count < 2 || target10Count > 6 {
		t.Errorf("target-10 should get ~20%% traffic (2-6 requests), got %d", target10Count)
	}

	t.Logf("Target distribution: target-9=%d (80%%), target-10=%d (20%%)", target9Count, target10Count)
}

func TestServiceModelAwareRoundRobinVariousScenarios(t *testing.T) {
	tests := []struct {
		name          string
		targetConfigs []struct {
			name     string
			keyCount int
			models   []string
		}
		requestCount int
		expectedDist map[string]float64 // target name -> expected percentage
		tolerance    float64            // allowed deviation from expected percentage
	}{
		{
			name: "extreme imbalance 10:1",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []string
			}{
				{"target-heavy", 10, []string{"qwen3.7-max"}},
				{"target-light", 1, []string{"qwen3.7-max"}},
			},
			requestCount: 22,
			expectedDist: map[string]float64{
				"target-heavy": 90.9, // 10/11
				"target-light": 9.1,  // 1/11
			},
			tolerance: 5.0,
		},
		{
			name: "three targets 3:2:1",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []string
			}{
				{"target-a", 3, []string{"qwen3.7-max"}},
				{"target-b", 2, []string{"qwen3.7-max"}},
				{"target-c", 1, []string{"qwen3.7-max"}},
			},
			requestCount: 30,
			expectedDist: map[string]float64{
				"target-a": 50.0, // 3/6
				"target-b": 33.3, // 2/6
				"target-c": 16.7, // 1/6
			},
			tolerance: 8.0,
		},
		{
			name: "five targets equal",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []string
			}{
				{"target-1", 1, []string{"qwen3.7-max"}},
				{"target-2", 1, []string{"qwen3.7-max"}},
				{"target-3", 1, []string{"qwen3.7-max"}},
				{"target-4", 1, []string{"qwen3.7-max"}},
				{"target-5", 1, []string{"qwen3.7-max"}},
			},
			requestCount: 25,
			expectedDist: map[string]float64{
				"target-1": 20.0,
				"target-2": 20.0,
				"target-3": 20.0,
				"target-4": 20.0,
				"target-5": 20.0,
			},
			tolerance: 8.0,
		},
		{
			name: "single target with multiple keys",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []string
			}{
				{"target-only", 5, []string{"qwen3.7-max"}},
			},
			requestCount: 10,
			expectedDist: map[string]float64{
				"target-only": 100.0,
			},
			tolerance: 0.0,
		},
		{
			name: "mixed models with filtering",
			targetConfigs: []struct {
				name     string
				keyCount int
				models   []string
			}{
				{"target-a", 2, []string{"qwen3.7-max"}},
				{"target-b", 3, []string{"other-model"}}, // should be filtered out
				{"target-c", 1, []string{"qwen3.7-max"}},
				{"target-d", 4, []string{"another-model"}}, // should be filtered out
			},
			requestCount: 15,
			expectedDist: map[string]float64{
				"target-a": 66.7, // 2/3
				"target-c": 33.3, // 1/3
			},
			tolerance: 10.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 mock 上游
			var servers []*httptest.Server
			for i := 0; i < len(tt.targetConfigs); i++ {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"model":"qwen3.7-max","usage":{"prompt_tokens":10,"completion_tokens":5}}`))
				}))
				defer server.Close()
				servers = append(servers, server)
			}

			// 配置 targets
			targets := make([]config.Target, len(tt.targetConfigs))
			for i, tc := range tt.targetConfigs {
				targets[i] = config.Target{
					Name:               tc.name,
					Endpoint:           servers[i].URL,
					ResourcePathPrefix: "/",
					APIKey:             fmt.Sprintf("key-%d", i),
					AllowedModels:      tc.models,
				}
				// 添加额外的 keys
				if tc.keyCount > 1 {
					extraKeys := make([]string, tc.keyCount-1)
					for j := 0; j < tc.keyCount-1; j++ {
						extraKeys[j] = fmt.Sprintf("key-%d-%d", i, j+2)
					}
					targets[i].APIKeys = extraKeys
				}
			}

			cfg := &config.Config{
				Server: config.ServerConfig{
					Bind:                  "127.0.0.1:0",
					RequestTimeoutSeconds: 5,
				},
				Targets: targets,
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

			// 创建客户端
			store := auth.NewStore()
			clients := make([]config.Client, tt.requestCount)
			for i := 0; i < tt.requestCount; i++ {
				clients[i] = config.Client{
					Name:      fmt.Sprintf("client-%d", i),
					AccessKey: fmt.Sprintf("token-%d", i),
				}
			}
			if err := store.LoadFromConfig(clients); err != nil {
				t.Fatalf("load clients: %v", err)
			}

			// 发送请求并统计分布
			targetCount := make(map[string]int)
			for i := 0; i < tt.requestCount; i++ {
				principal, ok := store.Authenticate(fmt.Sprintf("token-%d", i))
				if !ok {
					t.Fatalf("failed to authenticate client-%d", i)
				}

				body := bytes.NewBufferString(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
				req.RemoteAddr = fmt.Sprintf("192.168.%d.%d:12345", i/256, i%256)
				req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

				rr := httptest.NewRecorder()
				service.ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Fatalf("client-%d: expected 200, got %d", i, rr.Code)
				}

				target := rr.Header().Get("X-Proxy-Target")
				targetCount[target]++
			}

			// 验证分布
			for targetName, expectedPct := range tt.expectedDist {
				actualCount := targetCount[targetName]
				actualPct := float64(actualCount) / float64(tt.requestCount) * 100.0

				if actualPct < expectedPct-tt.tolerance || actualPct > expectedPct+tt.tolerance {
					t.Errorf("%s: expected ~%.1f%% (±%.1f%%), got %.1f%% (%d/%d requests)",
						targetName, expectedPct, tt.tolerance, actualPct, actualCount, tt.requestCount)
				}
			}

			// 验证没有意外的 targets 被选中
			for targetName := range targetCount {
				if _, expected := tt.expectedDist[targetName]; !expected {
					t.Errorf("unexpected target %s was selected", targetName)
				}
			}

			t.Logf("Distribution: %v", targetCount)
		})
	}
}

func TestServiceFailoverOnTransportError(t *testing.T) {
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mock":"ok"}`))
	}))
	defer success.Close()

	successURL, err := url.Parse(success.URL)
	if err != nil {
		t.Fatalf("parse success URL: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				APIKey:             "primary-key",
			},
			{
				Name:               "secondary",
				Endpoint:           success.URL,
				ResourcePathPrefix: "/",
				APIKey:             "secondary-key",
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
	service.httpClient = &http.Client{
		Transport: &failingTransport{successHost: successURL.Host},
	}
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected authenticated principal")
	}

	body := bytes.NewBufferString(`{"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	res := rr.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", res.StatusCode)
	}
	if got := res.Header.Get("X-Azure-Target"); got != "secondary" {
		t.Fatalf("expected target header to be secondary, got %q", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRequests != 1 {
		t.Fatalf("unexpected total requests: %d", metrics.TotalRequests)
	}
	if metrics.TotalSuccess != 1 {
		t.Fatalf("unexpected total success: %d", metrics.TotalSuccess)
	}
	if metrics.TotalFailures != 0 {
		t.Fatalf("unexpected total failures: %d", metrics.TotalFailures)
	}
	if metrics.TotalRetries != 1 {
		t.Fatalf("expected 1 retry, got %d", metrics.TotalRetries)
	}
	if metrics.TotalTargetRetries != 1 {
		t.Fatalf("expected 1 target retry, got %d", metrics.TotalTargetRetries)
	}
	if metrics.TotalKeyRetries != 0 {
		t.Fatalf("expected 0 key retries, got %d", metrics.TotalKeyRetries)
	}

	statuses := service.TargetStatuses(time.Now())
	var primaryMuted bool
	for _, st := range statuses {
		if st.Name == "primary" {
			primaryMuted = st.Muted
		}
	}
	if !primaryMuted {
		t.Fatal("expected primary target to be muted after failure")
	}
}

func TestServiceRejectsUnauthorizedTarget(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				APIKey:             "key",
			},
			{
				Name:               "secondary",
				Endpoint:           "http://secondary.local",
				ResourcePathPrefix: "/",
				APIKey:             "key2",
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("team-alpha", "alpha", "primary")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("alpha")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Proxy-Target", "secondary")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 status, got %d", rr.Code)
	}
}

func TestServiceTimeoutDoesNotRetryOrMute(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 1,
		},
		Targets: []config.Target{
			{
				Name:               "slow",
				Endpoint:           slow.URL,
				ResourcePathPrefix: "/",
				APIKey:             "key",
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
	service.setRequestTimeout(50 * time.Millisecond)
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 due to timeout, got %d", rr.Code)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries != 0 {
		t.Fatalf("expected no retries on timeout, got %d", metrics.TotalRetries)
	}

	statuses := service.TargetStatuses(time.Now())
	for _, st := range statuses {
		if st.Name == "slow" && st.Muted {
			t.Fatalf("expected target not to be muted on timeout")
		}
	}
}

func TestServiceAllowsBearerPassthrough(t *testing.T) {
	const bearerHeader = "Bearer azure-token"
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		if seenAuth == "" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "bearer",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/",
				AllowBearer:        true,
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{}`))
	req.Header.Set(headerAzureAuthorization, bearerHeader)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("expected 200, got %d body=%s", rr.Code, string(body))
	}
	if seenAuth != bearerHeader {
		t.Fatalf("expected upstream auth %q, got %q", bearerHeader, seenAuth)
	}
}

func TestServiceRejectsDisallowedModel(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "restricted",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key",
				AllowedModels:      []string{"gpt-4o"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-3.5","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if upstreamCalled {
		t.Fatalf("expected request not to reach upstream")
	}
}

func TestServiceStripsAPIVersionAndInternalQueryParams(t *testing.T) {
	var seenVersion string
	var seenOther string
	var seenTarget string
	var seenAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		seenVersion = q.Get("api-version")
		if seenVersion == "" {
			seenVersion = q.Get("API-Version")
		}
		seenOther = q.Get("other")
		seenTarget = q.Get("target")
		seenAPIKey = q.Get("api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key",
				AllowedModels:      []string{"gpt-4o"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-4o/chat/completions?api-version=foo&API-Version=bar&target=primary&api-key=client&other=yes", bytes.NewBufferString(`{}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// Azure targets inject api-version=2025-04-01-preview for deployment-based paths
	// after stripping client-supplied versions.
	// The test target has no endpoint_type set → defaults to azure_openai → must see dated version.
	if seenVersion != "2025-04-01-preview" {
		t.Fatalf("expected api-version=2025-04-01-preview for azure_openai deployment path, got %q", seenVersion)
	}
	if seenOther != "yes" {
		t.Fatalf("expected other query param preserved, got %q", seenOther)
	}
	if seenTarget != "" {
		t.Fatalf("expected internal target query param stripped, got %q", seenTarget)
	}
	if seenAPIKey != "" {
		t.Fatalf("expected client api-key query param stripped, got %q", seenAPIKey)
	}
}

func TestServiceStripsUnsupportedFieldsForResponses(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "primary",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			AllowedModels:      []string{"gpt-5.2"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-5.2","input":"hi","prompt_cache_key":"session-a","prompt_cache_retention":"24h","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if _, ok := captured["prompt_cache_retention"]; ok {
		t.Fatalf("expected prompt_cache_retention to be stripped, got body=%v", captured)
	}
	if _, ok := captured["foo"]; ok {
		t.Fatalf("expected unknown field foo to be stripped, got body=%v", captured)
	}
	if got, ok := captured["prompt_cache_key"].(string); !ok || got != "session-a" {
		t.Fatalf("expected prompt_cache_key to be preserved, got %v", captured["prompt_cache_key"])
	}
}

func TestServiceStripsUnsupportedFieldsForChatCompletions(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "primary",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			AllowedModels:      []string{"gpt-5.2"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":16,"prompt_cache_key":"session-a","prompt_cache_retention":"24h","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if _, ok := captured["prompt_cache_retention"]; ok {
		t.Fatalf("expected prompt_cache_retention to be stripped, got body=%v", captured)
	}
	if _, ok := captured["foo"]; ok {
		t.Fatalf("expected unknown field foo to be stripped, got body=%v", captured)
	}
	if _, ok := captured["messages"]; !ok {
		t.Fatalf("expected messages to be preserved, got body=%v", captured)
	}
}

func TestServiceRoutesByModelToSupportingTarget(t *testing.T) {
	target1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t1")
	}))
	defer target1.Close()
	target2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t2")
	}))
	defer target2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
			{
				Name:               "t2",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				AllowedModels:      []string{"gpt-5.1"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-5.1","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-5.1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Azure-Target"); got != "t2" {
		t.Fatalf("expected target t2, got %q", got)
	}
}

func TestServiceReturnsErrorWhenModelMissingAndAllowlistsConfigured(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-4o"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewBufferString(`{}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestServiceRoundsRobinAcrossMatchingTargets(t *testing.T) {
	counts := map[string]int{}
	makeServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[name]++
			w.WriteHeader(http.StatusOK)
		}))
	}
	s1 := makeServer("t1")
	defer s1.Close()
	s2 := makeServer("t2")
	defer s2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           s1.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-5.1"},
			},
			{
				Name:               "t2",
				Endpoint:           s2.URL,
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				AllowedModels:      []string{"gpt-5.1"},
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

	// Use distinct client names to avoid affinity stickiness collapsing all
	// requests to a single target.
	for i := 0; i < 6; i++ {
		clientName := fmt.Sprintf("tester-%d", i)
		store := auth.NewStore()
		if err := store.LoadFromConfig(testAuthClients(clientName, "token")); err != nil {
			t.Fatalf("load clients: %v", err)
		}
		principal, ok := store.Authenticate("token")
		if !ok {
			t.Fatal("expected principal")
		}

		body := bytes.NewBufferString(`{"model":"gpt-5.1","input":"hi"}`)
		req := httptest.NewRequest(http.MethodPost, "/openai/deployments/gpt-5.1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	}

	if counts["t1"] == 0 || counts["t2"] == 0 {
		t.Fatalf("expected both targets to receive traffic, got %+v", counts)
	}
	if diff := counts["t1"] - counts["t2"]; diff < -2 || diff > 2 {
		t.Fatalf("expected roughly balanced distribution, got %+v", counts)
	}
}

func TestServiceListsDeploymentsLocally(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-4o", "gpt-5.1"},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				AllowedModels:      []string{"gpt-5.1", "gpt-4o-mini"},
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
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/openai/deployments?api-version=ignored", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Data []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"data"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
		HasMore bool   `json:"has_more"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("expected 3 models, got %d", len(resp.Data))
	}
	if resp.HasMore {
		t.Fatalf("expected has_more=false")
	}
	if resp.FirstID == "" || resp.LastID == "" {
		t.Fatalf("expected first/last ids to be set")
	}
}

func TestServiceListsModelsLocally(t *testing.T) {
	paths := []string{"/openai/models?api-version=ignored", "/v1/models?api-version=ignored", "/models"}
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-4o"},
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
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s expected 200, got %d", p, rr.Code)
		}
		var resp struct {
			Data []struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"data"`
		}
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("%s decode: %v", p, err)
		}
		if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
			t.Fatalf("%s unexpected data: %+v", p, resp.Data)
		}
	}
}

func TestServiceLocalModelsDoNotExposeCopilotModels(t *testing.T) {
	var copilotModelRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected copilot upstream path: %s", r.URL.Path)
		}
		copilotModelRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [{
				"id": "gpt-5.1",
				"vendor": "openai",
				"model_picker_enabled": true,
				"capabilities": {"type": "chat"}
			}]
		}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-4o"},
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

	dbPath := filepath.Join(t.TempDir(), "copilot.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	poolStore := nosql.NewCopilotPoolStore(db)
	accountStore := nosql.NewCopilotAccountStore(db, poolStore)
	if err := poolStore.Create(nosql.CopilotPool{
		Name:        "test-pool",
		ClientName:  "tester",
		MaxAccounts: 1,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := accountStore.Create(nosql.CopilotAccount{
		PoolName:              "test-pool",
		GitHubUsername:        "test-user",
		Status:                nosql.AccountStatusActive,
		OAuthToken:            "oauth-token",
		CopilotToken:          "copilot-token",
		CopilotTokenExpiresAt: time.Now().Add(time.Hour).Unix(),
		APIBaseURL:            upstream.URL,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	service.SetCopilotService(copilot.NewCopilotService(accountStore, poolStore, upstream.Client(), newTestLogger()))

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
		t.Fatalf("unexpected local models: %+v", resp.Data)
	}
	for _, item := range resp.Data {
		if strings.HasPrefix(item.ID, "Copilot ") {
			t.Fatalf("root model list exposed copilot model %q", item.ID)
		}
	}
	if got := copilotModelRequests.Load(); got != 0 {
		t.Fatalf("root model list should not query copilot models, got %d requests", got)
	}
}

func TestServiceListsModelsLocallyRespectsAllowedTargets(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				AllowedModels:      []string{"gpt-5.2"},
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
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token", "t1")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
		t.Fatalf("unexpected filtered models: %+v", resp.Data)
	}
}

func TestServiceListsModelsLocallyRespectsRequestedTargetFilter(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				APIKey:             "key2",
				AllowedModels:      []string{"gpt-5.2"},
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
	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, _ := store.Authenticate("token")

	req := httptest.NewRequest(http.MethodGet, "/v1/models?target=t2", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-5.2" {
		t.Fatalf("unexpected target-filtered models: %+v", resp.Data)
	}
}

func TestServiceRecordsUsageOnSuccessfulResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	usageStore := nosql.NewUsageStore(db)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "t1",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
		}},
		DataStore: config.DataStore{DBPath: filepath.Join(tmpDir, "test.db")},
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
	service.SetUsageRecorder(usageStore)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := authStore.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	events, err := usageStore.List(usage.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(events))
	}
	evt := events[0]
	if evt.ClientName != "tester" || evt.InputTokens != 11 || evt.OutputTokens != 7 || evt.CachedTokens != 3 {
		t.Fatalf("unexpected usage event: %+v", evt)
	}
}

// TestServiceUsageEventResolvesAlias 红线：客户端用 DeepSeek 兼容别名 "deepseek-chat"
// 调用时，记录的用量事件 model 字段必须被规范化为 "deepseek-v4-flash"，
// 否则下游 CostTable 查不到价格、用量统计也无法按真实模型聚合。
func TestServiceUsageEventResolvesAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"deepseek-chat","usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	tmpDir := t.TempDir()
	db, err := nosql.OpenDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	usageStore := nosql.NewUsageStore(db)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:         "ds1",
			EndpointType: config.EndpointTypeDeepSeek,
			Endpoint:     upstream.URL,
			APIKey:       "key",
		}},
		DataStore: config.DataStore{DBPath: filepath.Join(tmpDir, "test.db")},
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
	service.SetUsageRecorder(usageStore)

	authStore := auth.NewStore()
	if err := authStore.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := authStore.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	events, err := usageStore.List(usage.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list usage events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(events))
	}
	if got := events[0].Model; got != "deepseek-v4-flash" {
		t.Fatalf("expected model normalized to deepseek-v4-flash, got %q", got)
	}
}

func TestExtractUsageFromClaudeSSE(t *testing.T) {
	// Claude streaming responses split usage across two events:
	// - message_start: contains input_tokens under message.usage
	// - message_delta: contains output_tokens under usage
	sseBody := []byte(
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[],"model":"claude-opus-4-6","usage":{"input_tokens":2048,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":350}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, model, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found in Claude SSE")
	}
	if model != "claude-opus-4-6" {
		t.Fatalf("expected model claude-opus-4-6, got %q", model)
	}
	if tokens.InputTokens != 2048 {
		t.Fatalf("expected input_tokens=2048 (from message_start), got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 350 {
		t.Fatalf("expected output_tokens=350 (from message_delta), got %d", tokens.OutputTokens)
	}
}

func TestExtractUsageFromClaudeSSETailOnly(t *testing.T) {
	// Simulate tail-only capture: message_start is missing (fell outside buffer),
	// only message_delta remains. We should still get output_tokens.
	sseBody := []byte(
		"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found")
	}
	if tokens.OutputTokens != 500 {
		t.Fatalf("expected output_tokens=500, got %d", tokens.OutputTokens)
	}
	// input_tokens will be 0 since message_start is missing — best effort.
	if tokens.InputTokens != 0 {
		t.Fatalf("expected input_tokens=0 (message_start missing), got %d", tokens.InputTokens)
	}
}

func TestExtractUsageFromClaudeSSEWithCaching(t *testing.T) {
	// Claude prompt caching: message_start has cache_read_input_tokens,
	// message_delta has only input_tokens (non-cached portion).
	// We should merge: total input = input_tokens + cache_read + cache_creation.
	sseBody := []byte(
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"model":"claude-opus-4-6","usage":{"input_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":50000,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":3,"output_tokens":200}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, model, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found")
	}
	if model != "claude-opus-4-6" {
		t.Fatalf("expected model claude-opus-4-6, got %q", model)
	}
	// Total input = 3 (non-cached) + 50000 (cache_read) from message_start
	// message_delta has input_tokens=3 (no cache fields) → merged max = 50003
	if tokens.InputTokens != 50003 {
		t.Fatalf("expected input_tokens=50003 (3 + 50000 cache_read), got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 200 {
		t.Fatalf("expected output_tokens=200, got %d", tokens.OutputTokens)
	}
	if tokens.CachedTokens != 50000 {
		t.Fatalf("expected cached_tokens=50000, got %d", tokens.CachedTokens)
	}
}

func TestExtractUsageFromClaudeSSECachingTailOnly(t *testing.T) {
	// When only message_delta is in buffer (long conversation), input_tokens
	// reflects only non-cached tokens. cache fields are missing.
	sseBody := []byte(
		"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":400}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	)

	tokens, _, ok := extractUsageFromSSE(sseBody)
	if !ok {
		t.Fatal("expected usage to be found")
	}
	// Without cache fields, input_tokens stays as 1 (best effort)
	if tokens.InputTokens != 1 {
		t.Fatalf("expected input_tokens=1, got %d", tokens.InputTokens)
	}
	if tokens.OutputTokens != 400 {
		t.Fatalf("expected output_tokens=400, got %d", tokens.OutputTokens)
	}
}

func TestExtractModelFromFormEncoded(t *testing.T) {
	body := []byte("model=gpt-4o&input=hello")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if got := extractModel(req, body); got != "gpt-4o" {
		t.Fatalf("expected model gpt-4o, got %q", got)
	}
}

func TestExtractModelFromMultipartForm(t *testing.T) {
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	if err := writer.WriteField("model", "gpt-image-1"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("prompt", "draw a cat"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	body := payload.Bytes()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", writer.FormDataContentType())

	if got := extractModel(req, body); got != "gpt-image-1" {
		t.Fatalf("expected model gpt-image-1, got %q", got)
	}
}

func TestExtractModelFromGeminiPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1beta/models/gemini-3.1-flash-image-preview:generateContent", "gemini-3.1-flash-image-preview"},
		{"/v1beta/models/gemini-3-pro-image-preview:streamGenerateContent", "gemini-3-pro-image-preview"},
		{"/v1alpha/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro"},
		{"/v1/models/some-model:countTokens", "some-model"},
		{"/v1beta/models/gemini-flash:generateContent?key=abc", "gemini-flash"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.Header.Set("Content-Type", "application/json")
		if got := extractModel(req, nil); got != tc.want {
			t.Errorf("path=%q: expected model %q, got %q", tc.path, tc.want, got)
		}
	}
}

func TestServiceOpenAITargetSendsBearerAuth(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "openai",
			EndpointType:  "openai",
			Endpoint:      upstream.URL,
			APIKey:        "sk-test-key",
			AllowedModels: []string{"gpt-4o"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenAuth != "Bearer sk-test-key" {
		t.Fatalf("expected Authorization 'Bearer sk-test-key', got %q", seenAuth)
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "openai" {
		t.Fatalf("expected X-Proxy-Target 'openai', got %q", got)
	}
	// backward-compat header should also be set
	if got := rr.Header().Get("X-Azure-Target"); got != "openai" {
		t.Fatalf("expected X-Azure-Target 'openai', got %q", got)
	}
}

func TestServiceClaudeTargetSendsXAPIKey(t *testing.T) {
	var seenAPIKey string
	var seenVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("x-api-key")
		seenVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "claude",
			EndpointType:  "claude",
			Endpoint:      upstream.URL,
			APIKey:        "sk-ant-test",
			AllowedModels: []string{"claude-sonnet-4-20250514"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenAPIKey != "sk-ant-test" {
		t.Fatalf("expected x-api-key 'sk-ant-test', got %q", seenAPIKey)
	}
	if seenVersion != "2023-06-01" {
		t.Fatalf("expected anthropic-version '2023-06-01', got %q", seenVersion)
	}
}

func TestServiceRoutesSameModelByRequestSchema(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "openai-shared",
				EndpointType:  "openai",
				Endpoint:      upstream.URL,
				APIKey:        "sk-openai",
				AllowedModels: []string{"shared-model"},
			},
			{
				Name:          "claude-shared",
				EndpointType:  "claude",
				Endpoint:      upstream.URL,
				APIKey:        "sk-claude",
				AllowedModels: []string{"shared-model"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	send := func(path string) *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, path, body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, rr.Code, rr.Body.String())
		}
		return rr
	}

	if got := send("/v1/chat/completions").Header().Get("X-Proxy-Target"); got != "openai-shared" {
		t.Fatalf("expected OpenAI Chat request to use openai-shared, got %q", got)
	}
	if got := send("/v1/messages").Header().Get("X-Proxy-Target"); got != "claude-shared" {
		t.Fatalf("expected Anthropic request to use claude-shared, got %q", got)
	}
}

func TestServiceRoutesResponsesToResponsesCapableTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "bailian-token-plan",
				EndpointType:  "bailian",
				Endpoint:      upstream.URL,
				APIKey:        "sk-bailian-token",
				AllowedModels: []string{"shared-model"},
			},
			{
				Name:          "bailian-api",
				EndpointType:  "bailian_api",
				Endpoint:      upstream.URL,
				APIKey:        "sk-bailian-api",
				AllowedModels: []string{"shared-model"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"shared-model","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// Both bailian and bailian_api now support /v1/responses, so either target is valid.
	if got := rr.Header().Get("X-Proxy-Target"); got != "bailian-api" && got != "bailian-token-plan" {
		t.Fatalf("expected Responses request to use a bailian target, got %q", got)
	}
}

func TestServiceRejectsModelWhenNoTargetSupportsRequestSchema(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "claude-only",
			EndpointType:  "claude",
			Endpoint:      upstream.URL,
			APIKey:        "sk-claude",
			AllowedModels: []string{"shared-model"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `no target supports path "/v1/chat/completions" for model "shared-model"`) {
		t.Fatalf("unexpected error body: %q", rr.Body.String())
	}
}

func TestServiceOpenAITargetSkipsSanitize(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "openai",
			EndpointType:  "openai",
			Endpoint:      upstream.URL,
			APIKey:        "sk-test",
			AllowedModels: []string{"gpt-5.2"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// Send fields that would be stripped for Azure (e.g. "foo" is not whitelisted)
	body := bytes.NewBufferString(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}],"custom_field":"keep-me","foo":"bar"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	// For OpenAI targets, fields should NOT be stripped
	if _, ok := captured["custom_field"]; !ok {
		t.Fatalf("expected custom_field to be preserved for openai target, got body=%v", captured)
	}
	if _, ok := captured["foo"]; !ok {
		t.Fatalf("expected foo to be preserved for openai target, got body=%v", captured)
	}
}

func TestServiceGeminiTargetSendsGoogAPIKey(t *testing.T) {
	var seenGoogKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenGoogKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:          "gemini",
			EndpointType:  "gemini",
			Endpoint:      upstream.URL,
			APIKey:        "AIza-test-key",
			AllowedModels: []string{"gemini-2.5-pro"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenGoogKey != "AIza-test-key" {
		t.Fatalf("expected x-goog-api-key 'AIza-test-key', got %q", seenGoogKey)
	}
}

func TestServiceClaudeGatewaySubPathPreserved(t *testing.T) {
	// Verify that when an endpoint has a sub-path (e.g. a Cloudflare AI Gateway
	// URL like /v2/gws/<id>/anthropic), the client's request path (/v1/messages)
	// is appended rather than replacing the endpoint path entirely.
	var seenPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/gws/testid/anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	// Build endpoint with gateway sub-path (no trailing /v1/messages — just the base).
	gatewayBase := upstream.URL + "/v2/gws/testid/anthropic"

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:         "claude-gw",
			EndpointType: "claude",
			Endpoint:     gatewayBase,
			APIKey:       "sk-ant-test",
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"claude-opus-4-5","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: body=%s", rr.Code, rr.Body.String())
	}
	if seenPath != "/v2/gws/testid/anthropic/v1/messages" {
		t.Fatalf("expected gateway sub-path preserved, got %q", seenPath)
	}
}

func TestServiceTargetDefaultEndpointType(t *testing.T) {
	// When endpoint_type is empty, it should default to azure_openai and use api-key header.
	var seenAPIKey string
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAPIKey = r.Header.Get("api-key")
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "azure-default",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "azure-key-123",
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"input":"hi"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if seenAPIKey != "azure-key-123" {
		t.Fatalf("expected api-key 'azure-key-123', got %q", seenAPIKey)
	}
	if seenAuth != "" {
		t.Fatalf("expected no Authorization header for default azure target, got %q", seenAuth)
	}
}

func TestService503PassThroughNoRetry(t *testing.T) {
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "image",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
			AllowedModels:      []string{"gpt-image-1"},
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-image-1","prompt":"draw a cat"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 pass-through, got %d", rr.Code)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call (no same-target retry), got %d", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries != 0 {
		t.Fatalf("expected no retries on 503 pass-through, got %d", metrics.TotalRetries)
	}
}

func TestService429PassThrough(t *testing.T) {
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 pass-through, got %d", rr.Code)
	}
	// 429 限流会重试 2 次，共 3 次上游调用
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 upstream calls (1 + 2 retries), got %d", got)
	}
	if got := rr.Result().Header.Get("Retry-After"); got != "42" {
		t.Fatalf("expected Retry-After header to be passed through, got %q", got)
	}

	metrics := service.MetricsSnapshot()
	// 2 次 429 重试（totalKeyRetries 计数）
	if metrics.TotalRetries != 2 {
		t.Fatalf("expected 2 retries on 429, got %d", metrics.TotalRetries)
	}
}

func TestService429RetrySuccess(t *testing.T) {
	// 第一次 429，第二次 200 — 验证重试机制能恢复
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{{
			Name:               "openai",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			APIKey:             "key",
		}},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", rr.Code)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected 2 upstream calls (1 x 429 + 1 x 200), got %d", got)
	}
}

func TestServiceFailoverOnNetworkErrorPreserved(t *testing.T) {
	// 守护测试：网络错误（连接拒绝）下仍应触发多-target failover。
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer success.Close()

	successURL, err := url.Parse(success.URL)
	if err != nil {
		t.Fatalf("parse success URL: %v", err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				APIKey:             "primary-key",
			},
			{
				Name:               "secondary",
				Endpoint:           success.URL,
				ResourcePathPrefix: "/",
				APIKey:             "secondary-key",
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
	service.httpClient = &http.Client{
		Transport: &failingTransport{successHost: successURL.Host},
	}
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"input":"hello"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after failover to secondary, got %d", rr.Code)
	}
	if got := rr.Result().Header.Get("X-Proxy-Target"); got != "secondary" {
		t.Fatalf("expected X-Proxy-Target=secondary, got %q", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries < 1 {
		t.Fatalf("expected at least 1 failover retry, got %d", metrics.TotalRetries)
	}
	if metrics.TotalTargetRetries < 1 {
		t.Fatalf("expected at least 1 target retry, got %d", metrics.TotalTargetRetries)
	}
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func testAuthClients(name, accessKey string, allowedTargets ...string) []config.Client {
	client := config.Client{
		Name:      name,
		AccessKey: accessKey,
	}
	if len(allowedTargets) > 0 {
		client.AllowedTargets = append([]string(nil), allowedTargets...)
	}
	return []config.Client{client}
}

func TestSelectTargetPathFiltering(t *testing.T) {
	t1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t1")
		w.WriteHeader(http.StatusOK)
	}))
	defer t1.Close()
	t2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "t2")
		w.WriteHeader(http.StatusOK)
	}))
	defer t2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "openai-t1",
				EndpointType:  "openai",
				Endpoint:      t1.URL,
				APIKey:        "key1",
				AllowedModels: []string{"gpt-4o"},
			},
			{
				Name:          "wangsu-t2",
				EndpointType:  "wangsu_openai",
				Endpoint:      t2.URL,
				APIKey:        "key2",
				AllowedModels: []string{"gpt-4o"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// /v1/responses → only openai-t1 supports this
	body := bytes.NewBufferString(`{"model":"gpt-4o","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/responses expected 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Proxy-Target"); got != "openai-t1" {
		t.Fatalf("/v1/responses expected target openai-t1, got %q", got)
	}

	// /v1/chat/completions → both support this, just ensure it succeeds
	body2 := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body2)
	req2 = req2.WithContext(auth.WithPrincipal(req2.Context(), principal))
	rr2 := httptest.NewRecorder()
	service.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("/v1/chat/completions expected 200, got %d", rr2.Code)
	}
}

func TestSelectTargetAffinity(t *testing.T) {
	counts := map[string]int{}
	makeServer := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[name]++
			w.WriteHeader(http.StatusOK)
		}))
	}
	s1 := makeServer("t1")
	defer s1.Close()
	s2 := makeServer("t2")
	defer s2.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "t1",
				EndpointType:  "openai",
				Endpoint:      s1.URL,
				APIKey:        "key1",
				AllowedModels: []string{"gpt-4o"},
			},
			{
				Name:          "t2",
				EndpointType:  "openai",
				Endpoint:      s2.URL,
				APIKey:        "key2",
				AllowedModels: []string{"gpt-4o"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// First request establishes affinity
	body := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rr.Code)
	}
	firstTarget := rr.Header().Get("X-Proxy-Target")

	// Subsequent requests should stick to the same target
	for i := 0; i < 5; i++ {
		body := bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
		rr := httptest.NewRecorder()
		service.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+2, rr.Code)
		}
		if got := rr.Header().Get("X-Proxy-Target"); got != firstTarget {
			t.Fatalf("request %d: expected sticky target %q, got %q", i+2, firstTarget, got)
		}
	}
}

func TestSelectTargetExplicitPathIncompatible(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "wangsu",
				EndpointType:  "wangsu_openai",
				Endpoint:      "http://example.com",
				APIKey:        "key",
				AllowedModels: []string{"gpt-4o"},
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	body := bytes.NewBufferString(`{"model":"gpt-4o","input":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("X-Proxy-Target", "wangsu")
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for path-incompatible explicit target, got %d", rr.Code)
	}
}

func TestServiceStartTime(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: "logs/test-access.log",
			ErrorLog:  "logs/test-error.log",
		},
	}

	before := time.Now()
	service, err := NewService(cfg, newTestLogger())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	after := time.Now()

	startTime := service.StartTime()
	if startTime.Before(before) || startTime.After(after) {
		t.Errorf("StartTime %v not between %v and %v", startTime, before, after)
	}
}

func TestServiceKeyPoolStatus(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "with-pool",
				Endpoint:      "http://example.com",
				APIKey:        "key1",
				APIKeys:       []string{"key2", "key3"},
				AllowedModels: []string{"gpt-4"},
			},
			{
				Name:          "no-pool",
				Endpoint:      "http://example.com",
				APIKey:        "single-key",
				AllowedModels: []string{"gpt-4"},
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

	// Target with key pool
	status := service.KeyPoolStatus("with-pool")
	if status == nil {
		t.Fatal("expected key pool status for 'with-pool'")
	}
	if len(status) != 3 {
		t.Errorf("expected 3 keys, got %d", len(status))
	}

	// Target without key pool (single key)
	status = service.KeyPoolStatus("no-pool")
	if status != nil {
		t.Errorf("expected nil for single-key target, got %v", status)
	}

	// Non-existent target
	status = service.KeyPoolStatus("nonexistent")
	if status != nil {
		t.Errorf("expected nil for nonexistent target, got %v", status)
	}
}

func TestServiceBlockUnblockKey(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "target1",
				Endpoint:      "http://example.com",
				APIKey:        "key1",
				APIKeys:       []string{"key2", "key3"},
				AllowedModels: []string{"gpt-4"},
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

	// Block key
	err = service.BlockKey("target1", 0)
	if err != nil {
		t.Errorf("BlockKey failed: %v", err)
	}

	// Verify blocked
	status := service.KeyPoolStatus("target1")
	if !status[0].Blocked {
		t.Error("expected key 0 to be blocked")
	}

	// Unblock key
	err = service.UnblockKey("target1", 0)
	if err != nil {
		t.Errorf("UnblockKey failed: %v", err)
	}

	// Verify unblocked
	status = service.KeyPoolStatus("target1")
	if status[0].Blocked {
		t.Error("expected key 0 to be unblocked")
	}

	// Error cases
	err = service.BlockKey("nonexistent", 0)
	if err == nil {
		t.Error("expected error for nonexistent target")
	}

	err = service.BlockKey("target1", -1)
	if err == nil {
		t.Error("expected error for negative index")
	}

	err = service.BlockKey("target1", 100)
	if err == nil {
		t.Error("expected error for out-of-range index")
	}

	err = service.UnblockKey("nonexistent", 0)
	if err == nil {
		t.Error("expected error for nonexistent target")
	}

	err = service.UnblockKey("target1", -1)
	if err == nil {
		t.Error("expected error for negative index")
	}

	err = service.UnblockKey("target1", 100)
	if err == nil {
		t.Error("expected error for out-of-range index")
	}
}

func TestServiceTraceMethods(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
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

	// Without trace store enabled, methods should return empty/nil results
	record := service.GetTrace("any-id")
	if record != nil {
		t.Error("expected nil when trace store disabled")
	}

	records := service.ListTrace(10)
	if len(records) != 0 {
		t.Errorf("expected empty list when trace store disabled, got %d records", len(records))
	}

	stats := service.TraceStats()
	if stats != nil {
		// Stats may return non-nil but with zero values when disabled
		if stats["total_records"] != 0 {
			t.Errorf("expected 0 total_records when disabled, got %d", stats["total_records"])
		}
	}
}

func TestServiceUpdateTargets(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:          "target1",
				Endpoint:      "http://example1.com",
				APIKey:        "key1",
				AllowedModels: []string{"gpt-4"},
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

	// Update with new targets
	newTargets := []config.Target{
		{
			Name:          "target2",
			Endpoint:      "http://example2.com",
			APIKey:        "key2",
			AllowedModels: []string{"gpt-3.5"},
		},
		{
			Name:          "target3",
			Endpoint:      "http://example3.com",
			APIKey:        "key3",
			AllowedModels: []string{"gpt-4"},
		},
	}

	err = service.UpdateTargets(newTargets)
	if err != nil {
		t.Errorf("UpdateTargets failed: %v", err)
	}

	// Verify old target is gone
	status := service.KeyPoolStatus("target1")
	if status != nil {
		t.Error("expected target1 to be removed")
	}

	// Verify new targets exist
	statuses := service.TargetStatuses(time.Now())
	if len(statuses) != 2 {
		t.Errorf("expected 2 targets, got %d", len(statuses))
	}
}

func TestForwardAttemptError(t *testing.T) {
	// Test Error method
	err := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: true,
		err:       fmt.Errorf("connection refused"),
		startedAt: time.Now(),
	}

	if err.Error() != "connection refused" {
		t.Errorf("expected 'connection refused', got %q", err.Error())
	}

	// Test nil error
	var nilErr *forwardAttemptError
	if nilErr.Error() != "" {
		t.Errorf("expected empty string for nil error, got %q", nilErr.Error())
	}

	// Test nil inner error
	err2 := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: false,
		err:       nil,
		startedAt: time.Now(),
	}
	if err2.Error() != "" {
		t.Errorf("expected empty string for nil inner error, got %q", err2.Error())
	}

	// Test Unwrap method
	innerErr := fmt.Errorf("inner error")
	err3 := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: true,
		err:       innerErr,
		startedAt: time.Now(),
	}
	if err3.Unwrap() != innerErr {
		t.Error("Unwrap should return inner error")
	}

	// Test Unwrap with nil receiver
	var nilErr2 *forwardAttemptError
	if nilErr2.Unwrap() != nil {
		t.Error("Unwrap on nil receiver should return nil")
	}

	// Test Unwrap with nil inner error
	err4 := &forwardAttemptError{
		status:    http.StatusBadGateway,
		retryable: false,
		err:       nil,
		startedAt: time.Now(),
	}
	if err4.Unwrap() != nil {
		t.Error("Unwrap with nil inner error should return nil")
	}
}

func TestSelectionError(t *testing.T) {
	err := &selectionError{
		Status:  http.StatusForbidden,
		Message: "target not allowed",
	}

	if err.Error() != "target not allowed" {
		t.Errorf("expected 'target not allowed', got %q", err.Error())
	}
}

func TestServiceSetCopilotService(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
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

	// Initially nil
	if service.copilotService != nil {
		t.Error("expected copilotService to be nil initially")
	}

	// Set to nil (should not panic)
	service.SetCopilotService(nil)
	if service.copilotService != nil {
		t.Error("expected copilotService to remain nil")
	}
}

func TestFindAvailableTarget(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
				// No AllowedModels - accepts any model
			},
			{
				Name:     "target2",
				Endpoint: "http://example2.com",
				APIKey:   "key2",
				// No AllowedModels - accepts any model
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

	// Test with no filters
	state, muted := service.findAvailableTarget(nil, nil, time.Now())
	if state == nil {
		t.Error("expected to find available target")
	}
	if muted != nil {
		t.Error("expected no muted candidate")
	}

	// Test with allowed filter
	allowed := map[string]struct{}{"target2": {}}
	state, muted = service.findAvailableTarget(allowed, nil, time.Now())
	if state == nil {
		t.Error("expected to find target2")
	}
	if state.Target().Name != "target2" {
		t.Errorf("expected target2, got %s", state.Target().Name)
	}

	// Test with attempted filter (all targets attempted)
	attempted := map[string]struct{}{"target1": {}, "target2": {}}
	state, muted = service.findAvailableTarget(nil, attempted, time.Now())
	if state != nil {
		t.Error("expected no available target when all attempted")
	}
}

func TestHandleCopilotQuotaSummary_NoService(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
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

	store := auth.NewStore()
	if err := store.LoadFromConfig(testAuthClients("tester", "token")); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	// Test without copilot service configured
	req := httptest.NewRequest(http.MethodGet, "/copilot/quota", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))

	rr := httptest.NewRecorder()
	service.HandleCopilotQuotaSummary(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected 502 when copilot service not configured, got %d", rr.Code)
	}

	// Test without authentication
	req2 := httptest.NewRequest(http.MethodGet, "/copilot/quota", nil)
	rr2 := httptest.NewRecorder()
	service.HandleCopilotQuotaSummary(rr2, req2)

	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without authentication, got %d", rr2.Code)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		expected   string
	}{
		{
			name:       "nil request",
			remoteAddr: "",
			xff:        "",
			expected:   "",
		},
		{
			name:       "remote addr with port",
			remoteAddr: "192.168.1.1:8080",
			xff:        "",
			expected:   "192.168.1.1",
		},
		{
			name:       "remote addr without port",
			remoteAddr: "192.168.1.1",
			xff:        "",
			expected:   "192.168.1.1",
		},
		{
			name:       "X-Forwarded-For single IP",
			remoteAddr: "192.168.1.1:8080",
			xff:        "10.0.0.1",
			expected:   "10.0.0.1",
		},
		{
			name:       "X-Forwarded-For multiple IPs",
			remoteAddr: "192.168.1.1:8080",
			xff:        "10.0.0.1, 10.0.0.2, 10.0.0.3",
			expected:   "10.0.0.1",
		},
		{
			name:       "X-Forwarded-For with spaces",
			remoteAddr: "192.168.1.1:8080",
			xff:        "  10.0.0.1  ",
			expected:   "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.remoteAddr != "" || tt.xff != "" {
				req = httptest.NewRequest(http.MethodGet, "/", nil)
				req.RemoteAddr = tt.remoteAddr
				if tt.xff != "" {
					req.Header.Set("X-Forwarded-For", tt.xff)
				}
			}

			result := clientIP(req)
			if result != tt.expected {
				t.Errorf("clientIP() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "application/json")
	src.Set("X-Custom-Header", "value1")
	src.Add("X-Custom-Header", "value2")
	src.Set("Connection", "keep-alive") // hop header, should be skipped

	dst := http.Header{}
	copyHeaders(dst, src)

	// Check that non-hop headers are copied
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type not copied correctly")
	}

	// Check that multiple values are preserved
	values := dst.Values("X-Custom-Header")
	if len(values) != 2 {
		t.Errorf("expected 2 values for X-Custom-Header, got %d", len(values))
	}

	// Check that hop headers are skipped
	if dst.Get("Connection") != "" {
		t.Errorf("Connection header should be skipped")
	}
}

func TestTargetName(t *testing.T) {
	// Test with nil target
	if targetName(nil) != "" {
		t.Errorf("expected empty string for nil target")
	}

	// Test with valid target
	target := &Target{Name: "test-target"}
	if targetName(target) != "test-target" {
		t.Errorf("expected 'test-target', got %q", targetName(target))
	}
}

func TestTargetEndpointType(t *testing.T) {
	// Test with nil target
	if targetEndpointType(nil) != "" {
		t.Errorf("expected empty string for nil target")
	}

	// Test with valid target
	target := &Target{EndpointType: "openai"}
	if targetEndpointType(target) != "openai" {
		t.Errorf("expected 'openai', got %q", targetEndpointType(target))
	}
}

func TestStripURLQuery(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http://example.com/path", "http://example.com/path"},
		{"http://example.com/path?query=value", "http://example.com/path"},
		{"http://example.com/path?query=value&foo=bar", "http://example.com/path"},
		{"http://example.com/path?", "http://example.com/path"},
		{"", ""},
	}

	for _, tt := range tests {
		result := stripURLQuery(tt.input)
		if result != tt.expected {
			t.Errorf("stripURLQuery(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestClassifyTransportError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{
			name:     "context deadline exceeded",
			err:      context.DeadlineExceeded,
			expected: http.StatusGatewayTimeout,
		},
		{
			name:     "context canceled",
			err:      context.Canceled,
			expected: http.StatusGatewayTimeout,
		},
		{
			name:     "generic error",
			err:      fmt.Errorf("connection refused"),
			expected: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyTransportError(tt.err)
			if result != tt.expected {
				t.Errorf("classifyTransportError() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestHandleForwardError(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
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

	// Get target state
	service.mu.RLock()
	state := service.targetsByName["target1"]
	service.mu.RUnlock()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Test with status 0 (should default to 502)
	service.handleForwardError(req, state, fmt.Errorf("connection refused"), 0)

	// Verify target is marked as failed
	stats := state.Stats()
	if stats.ConsecutiveFailure == 0 {
		t.Error("expected target to be marked as failed")
	}

	// Test with explicit status
	service.handleForwardError(req, state, fmt.Errorf("timeout"), http.StatusGatewayTimeout)
}

func TestWriteForwardErrorLog(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
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

	// Get target state
	service.mu.RLock()
	state := service.targetsByName["target1"]
	service.mu.RUnlock()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Test with nil error (should not panic)
	writeForwardErrorLog(req, state, nil)

	// Test with valid error
	fe := &forwardAttemptError{
		status:          http.StatusBadGateway,
		retryable:       true,
		err:             fmt.Errorf("connection refused"),
		startedAt:       time.Now().Add(-100 * time.Millisecond),
		upstreamURL:     "http://example.com/v1/chat",
		upstreamFullURL: "http://example.com/v1/chat?api-version=2024-01-01",
	}
	writeForwardErrorLog(req, state, fe)
}

func TestWriteUpstreamErrorLog(t *testing.T) {
	target := &Target{
		Name:         "target1",
		EndpointType: "openai",
		Endpoint:     mustParseURL("http://example.com"),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Test with 4xx response
	resp4xx := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{},
	}
	writeUpstreamErrorLog(req, target, resp4xx, []byte("error message"), time.Now().Add(-100*time.Millisecond))

	// Test with 5xx response
	resp5xx := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{},
	}
	writeUpstreamErrorLog(req, target, resp5xx, []byte("server error"), time.Now().Add(-100*time.Millisecond))

	// Test with nil target
	writeUpstreamErrorLog(req, nil, resp4xx, []byte("error"), time.Now())

	// Test with large body (should truncate)
	largeBody := make([]byte, 2000)
	for i := range largeBody {
		largeBody[i] = 'x'
	}
	writeUpstreamErrorLog(req, target, resp4xx, largeBody, time.Now())

	// Test with gzip compressed body
	var compressedBody bytes.Buffer
	gw := gzip.NewWriter(&compressedBody)
	gw.Write([]byte("compressed error message"))
	gw.Close()

	respGzip := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	writeUpstreamErrorLog(req, target, respGzip, compressedBody.Bytes(), time.Now())
}

func mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func TestRecordUsageEvent(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
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

	// Test with nil recorder (should not panic)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	service.recordUsageEvent(req, nil, 200, "gpt-4", "application/json", nil, nil, -1)

	// Test with nil request (should not panic)
	service.recordUsageEvent(nil, nil, 200, "gpt-4", "application/json", nil, nil, -1)

	// Test with valid request but no principal
	service.recordUsageEvent(req, nil, 200, "gpt-4", "application/json", []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`), nil, -1)
}

func TestRecordTrace(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		Targets: []config.Target{
			{
				Name:     "target1",
				Endpoint: "http://example1.com",
				APIKey:   "key1",
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

	// Test with nil traceStore (should not panic)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	target := &Target{Name: "target1", EndpointType: "openai"}
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
	}
	service.recordTrace(req, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with nil target
	service.recordTrace(req, nil, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), -1, "gpt-4")

	// Test with gzip compressed response body
	var compressedBody bytes.Buffer
	gw := gzip.NewWriter(&compressedBody)
	gw.Write([]byte(`{"id":"compressed-test"}`))
	gw.Close()

	respGzip := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	service.recordTrace(req, target, respGzip, []byte(`{"model":"gpt-4"}`), compressedBody.Bytes(), time.Now(), 0, "gpt-4")

	// Test with target that has APIKeys
	targetWithKeys := &Target{
		Name:         "target-with-keys",
		EndpointType: "openai",
		APIKey:       "main-key",
		APIKeys:      []string{"key1", "key2", "key3"},
	}
	service.recordTrace(req, targetWithKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 1, "gpt-4")

	// Test with target that has only APIKey (no APIKeys)
	targetSingleKey := &Target{
		Name:         "target-single-key",
		EndpointType: "openai",
		APIKey:       "single-key",
	}
	service.recordTrace(req, targetSingleKey, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has sensitive headers
	reqWithHeaders := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqWithHeaders.Header.Set("Authorization", "Bearer secret-token")
	reqWithHeaders.Header.Set("X-API-Key", "secret-api-key")
	reqWithHeaders.Header.Set("X-Goog-API-Key", "secret-goog-key")
	reqWithHeaders.Header.Set("Cookie", "session=secret-session")
	reqWithHeaders.Header.Set("Content-Type", "application/json")
	service.recordTrace(reqWithHeaders, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has sensitive headers
	respWithHeaders := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Authorization":  []string{"Bearer upstream-token"},
			"X-Api-Key":      []string{"upstream-api-key"},
			"X-Goog-Api-Key": []string{"upstream-goog-key"},
			"Set-Cookie":     []string{"session=upstream-session"},
			"Content-Type":   []string{"application/json"},
		},
	}
	service.recordTrace(req, target, respWithHeaders, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with sensitive query params
	reqForURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api-key=secret&apikey=secret2&key=secret3&api_key=secret4&other=safe", nil)
	respWithReq := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqForURL,
	}
	service.recordTrace(req, target, respWithReq, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with target that has empty APIKeys and empty APIKey
	targetNoKeys := &Target{
		Name:         "target-no-keys",
		EndpointType: "openai",
	}
	service.recordTrace(req, targetNoKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with keyIndex out of range
	service.recordTrace(req, targetWithKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 999, "gpt-4")

	// Test with response that has request URL with no query params
	reqNoQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	respNoQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqNoQuery,
	}
	service.recordTrace(req, target, respNoQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with safe query params only
	reqSafeQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model=gpt-4&temperature=0.7", nil)
	respSafeQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqSafeQuery,
	}
	service.recordTrace(req, target, respSafeQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has nil request
	respNilReq := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    nil,
	}
	service.recordTrace(req, target, respNilReq, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with nil URL
	reqNilURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqNilURL.URL = nil
	respNilURL := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqNilURL,
	}
	service.recordTrace(req, target, respNilURL, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has empty response body
	service.recordTrace(req, target, resp, []byte(`{"model":"gpt-4"}`), []byte{}, time.Now(), 0, "gpt-4")

	// Test with response that has nil response body
	service.recordTrace(req, target, resp, []byte(`{"model":"gpt-4"}`), nil, time.Now(), 0, "gpt-4")

	// Test with response that has gzip header but empty body
	respGzipEmpty := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	service.recordTrace(req, target, respGzipEmpty, []byte(`{"model":"gpt-4"}`), []byte{}, time.Now(), 0, "gpt-4")

	// Test with response that has gzip header but invalid gzip body
	respGzipInvalid := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Encoding": []string{"gzip"}},
	}
	service.recordTrace(req, target, respGzipInvalid, []byte(`{"model":"gpt-4"}`), []byte("not-gzip-data"), time.Now(), 0, "gpt-4")

	// Test with request that has no headers
	reqNoHeaders := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqNoHeaders.Header = http.Header{}
	service.recordTrace(reqNoHeaders, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has no headers
	respNoHeaders := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
	}
	service.recordTrace(req, target, respNoHeaders, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with target that has APIKey but empty APIKeys slice
	targetAPIKeyOnly := &Target{
		Name:         "target-apikey-only",
		EndpointType: "openai",
		APIKey:       "only-key",
		APIKeys:      []string{},
	}
	service.recordTrace(req, targetAPIKeyOnly, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with negative keyIndex
	service.recordTrace(req, targetWithKeys, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), -1, "gpt-4")

	// Test with request that has query params
	reqWithQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?foo=bar&baz=qux", nil)
	service.recordTrace(reqWithQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with mixed sensitive and safe query params
	reqMixedQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api-key=secret&model=gpt-4&temperature=0.7", nil)
	respMixedQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqMixedQuery,
	}
	service.recordTrace(req, target, respMixedQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has empty query params
	reqEmptyQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?", nil)
	service.recordTrace(reqEmptyQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with empty query params
	reqEmptyQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?", nil)
	respEmptyQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqEmptyQueryURL,
	}
	service.recordTrace(req, target, respEmptyQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has multiple headers with same key
	reqMultiHeaders := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqMultiHeaders.Header.Add("X-Custom", "value1")
	reqMultiHeaders.Header.Add("X-Custom", "value2")
	service.recordTrace(reqMultiHeaders, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has multiple headers with same key
	respMultiHeaders := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"X-Custom": []string{"value1", "value2"},
		},
	}
	service.recordTrace(req, target, respMultiHeaders, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has path with query params
	reqPathQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true", nil)
	service.recordTrace(reqPathQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with path and query params
	reqPathQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true&model=gpt-4", nil)
	respPathQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqPathQueryURL,
	}
	service.recordTrace(req, target, respPathQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has path with special characters
	reqSpecialPath := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?model=gpt-4&prompt=hello%20world", nil)
	service.recordTrace(reqSpecialPath, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with special characters in query
	reqSpecialQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?prompt=hello%20world&model=gpt-4", nil)
	respSpecialQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqSpecialQueryURL,
	}
	service.recordTrace(req, target, respSpecialQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has path with unicode characters
	reqUnicodePath := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?prompt=你好世界", nil)
	service.recordTrace(reqUnicodePath, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with unicode characters in query
	reqUnicodeQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?prompt=你好世界&model=gpt-4", nil)
	respUnicodeQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqUnicodeQueryURL,
	}
	service.recordTrace(req, target, respUnicodeQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has path with fragment
	reqFragmentPath := httptest.NewRequest(http.MethodPost, "/v1/chat/completions#section", nil)
	service.recordTrace(reqFragmentPath, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with fragment
	reqFragmentURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions#section", nil)
	respFragment := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqFragmentURL,
	}
	service.recordTrace(req, target, respFragment, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with request that has path with multiple query params
	reqMultiQuery := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?a=1&b=2&c=3", nil)
	service.recordTrace(reqMultiQuery, target, resp, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")

	// Test with response that has request URL with multiple query params
	reqMultiQueryURL := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?a=1&b=2&c=3", nil)
	respMultiQuery := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Request:    reqMultiQueryURL,
	}
	service.recordTrace(req, target, respMultiQuery, []byte(`{"model":"gpt-4"}`), []byte(`{"id":"test"}`), time.Now(), 0, "gpt-4")
}
