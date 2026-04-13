package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ycgame/llms-proxy/internal/auth"
	"github.com/ycgame/llms-proxy/internal/copilot"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// setupPassthroughTestEnv 创建一个带 Copilot 服务的测试 proxy.Service。
// upstream 是可选的 mock 上游 URL；如果非空，则设为 account 的 APIBaseURL。
// 返回 Service + 清理函数。
func setupPassthroughTestEnv(t *testing.T, upstreamURL string) *Service {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := nosql.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	poolStore := nosql.NewCopilotPoolStore(db)
	acctStore := nosql.NewCopilotAccountStore(db, poolStore)

	if err := poolStore.Create(nosql.CopilotPool{
		Name:        "test-pool",
		ClientName:  "test-client",
		MaxAccounts: 5,
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	apiBaseURL := ""
	if upstreamURL != "" {
		apiBaseURL = strings.TrimRight(upstreamURL, "/")
	}

	acct := nosql.CopilotAccount{
		PoolName:              "test-pool",
		GitHubUsername:        "test-user",
		Status:                nosql.AccountStatusActive,
		SortOrder:             1,
		QuotaPercentRemaining: 75.0,
		OAuthToken:            "gho_fake_oauth_token",
		CopilotToken:          "test-copilot-token-12345",
		CopilotTokenExpiresAt: time.Now().Unix() + 99999, // far future
		APIBaseURL:            apiBaseURL,
	}
	if err := acctStore.Create(acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	copilotSvc := copilot.NewCopilotService(acctStore, poolStore, nil, nil)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &Service{
		logger:         logger,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		targetsByName:  make(map[string]*targetState),
		affinity:       newAffinityMap(),
		copilotService: copilotSvc,
		startTime:      time.Now(),
	}
	svc.setRequestTimeout(30 * time.Second)

	return svc
}

// reqWithPrincipal 创建一个注入了 auth.Principal 的 HTTP request。
func reqWithPrincipal(t *testing.T, method, url string, body io.Reader, clientName string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, url, body)
	principal := &auth.Principal{Name: clientName}
	ctx := auth.WithPrincipal(r.Context(), principal)
	return r.WithContext(ctx)
}

// ---------- HandleCopilotAuth ----------

func TestHandleCopilotAuth_Success(t *testing.T) {
	svc := setupPassthroughTestEnv(t, "")

	r := reqWithPrincipal(t, http.MethodGet, "/copilot/auth", nil, "test-client")
	w := httptest.NewRecorder()

	svc.HandleCopilotAuth(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", body["status"])
	}
	if body["client"] != "test-client" {
		t.Fatalf("expected client=test-client, got %v", body["client"])
	}
	if body["pool"] != "test-pool" {
		t.Fatalf("expected pool=test-pool, got %v", body["pool"])
	}
	// 1 active account
	if body["accounts_available"] != float64(1) {
		t.Fatalf("expected accounts_available=1, got %v", body["accounts_available"])
	}
}

func TestHandleCopilotAuth_NoPool(t *testing.T) {
	svc := setupPassthroughTestEnv(t, "")

	r := reqWithPrincipal(t, http.MethodGet, "/copilot/auth", nil, "unknown-client")
	w := httptest.NewRecorder()

	svc.HandleCopilotAuth(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandleCopilotAuth_NoCopilotService(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &Service{
		logger:         logger,
		httpClient:     &http.Client{},
		targetsByName:  make(map[string]*targetState),
		affinity:       newAffinityMap(),
		copilotService: nil, // not configured
		startTime:      time.Now(),
	}

	r := reqWithPrincipal(t, http.MethodGet, "/copilot/auth", nil, "test-client")
	w := httptest.NewRecorder()

	svc.HandleCopilotAuth(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
}

// ---------- Path Mapping ----------

func TestCopilotPassthrough_PathMapping(t *testing.T) {
	tests := []struct {
		name         string
		requestPath  string
		expectedPath string
	}{
		{"chat completions", "/copilot/chat/completions", "/chat/completions"},
		{"v1 messages", "/copilot/v1/messages", "/v1/messages"},
		{"root", "/copilot", "/"},
		{"nested path", "/copilot/v1/chat/completions", "/v1/chat/completions"},
		{"embeddings", "/copilot/embeddings", "/embeddings"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()

			svc := setupPassthroughTestEnv(t, upstream.URL)

			r := reqWithPrincipal(t, http.MethodPost, tt.requestPath, strings.NewReader(`{"model":"test"}`), "test-client")
			w := httptest.NewRecorder()

			svc.HandleCopilotPassthrough(w, r)

			resp := w.Result()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
			}

			if capturedPath != tt.expectedPath {
				t.Fatalf("expected upstream path %q, got %q", tt.expectedPath, capturedPath)
			}
		})
	}
}

// ---------- Preserves Downstream Headers ----------

func TestCopilotPassthrough_PreservesDownstreamHeaders(t *testing.T) {
	headersToCheck := map[string]string{
		"X-Initiator":   "user-action",
		"User-Agent":    "my-custom-agent/1.0",
		"Openai-Intent": "conversation",
	}

	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	svc := setupPassthroughTestEnv(t, upstream.URL)

	r := reqWithPrincipal(t, http.MethodPost, "/copilot/chat/completions", strings.NewReader(`{}`), "test-client")
	for k, v := range headersToCheck {
		r.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	svc.HandleCopilotPassthrough(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	for k, expected := range headersToCheck {
		got := capturedHeaders.Get(k)
		if got != expected {
			t.Errorf("header %q: expected %q, got %q", k, expected, got)
		}
	}

	// Authorization should be replaced with copilot token
	authHeader := capturedHeaders.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer test-copilot-token-") {
		t.Errorf("Authorization should be replaced with copilot token, got %q", authHeader)
	}
}

// ---------- Editor Headers Fallback ----------

func TestCopilotPassthrough_EditorHeadersFallback(t *testing.T) {
	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	svc := setupPassthroughTestEnv(t, upstream.URL)

	// Case 1: 下游未提供 Editor headers → 代理补充默认值
	r := reqWithPrincipal(t, http.MethodPost, "/copilot/chat/completions", strings.NewReader(`{}`), "test-client")
	w := httptest.NewRecorder()
	svc.HandleCopilotPassthrough(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Editor headers MUST be present (GitHub upstream requires them)
	editorDefaults := map[string]string{
		"Editor-Version":         copilot.HeaderEditorVersion,
		"Editor-Plugin-Version":  copilot.HeaderPluginVersion,
		"Copilot-Integration-Id": copilot.HeaderIntegrationID,
	}
	for h, expected := range editorDefaults {
		if val := capturedHeaders.Get(h); val != expected {
			t.Errorf("editor header %q: expected default %q, got %q", h, expected, val)
		}
	}

	// Case 2: 下游提供了自定义 Editor headers → 保留下游值
	capturedHeaders = nil
	r2 := reqWithPrincipal(t, http.MethodPost, "/copilot/chat/completions", strings.NewReader(`{}`), "test-client")
	r2.Header.Set("Editor-Version", "custom-editor/1.0")
	r2.Header.Set("Copilot-Integration-Id", "custom-integration")
	w2 := httptest.NewRecorder()
	svc.HandleCopilotPassthrough(w2, r2)

	resp2 := w2.Result()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	// 下游自定义值应该被保留
	if val := capturedHeaders.Get("Editor-Version"); val != "custom-editor/1.0" {
		t.Errorf("Editor-Version: expected downstream value %q, got %q", "custom-editor/1.0", val)
	}
	if val := capturedHeaders.Get("Copilot-Integration-Id"); val != "custom-integration" {
		t.Errorf("Copilot-Integration-Id: expected downstream value %q, got %q", "custom-integration", val)
	}
	// 未由下游提供的 header 应该用默认值补充
	if val := capturedHeaders.Get("Editor-Plugin-Version"); val != copilot.HeaderPluginVersion {
		t.Errorf("Editor-Plugin-Version: expected default %q, got %q", copilot.HeaderPluginVersion, val)
	}
}

// ---------- Response Headers ----------

func TestCopilotPassthrough_ResponseHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Upstream", "some-value")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	svc := setupPassthroughTestEnv(t, upstream.URL)

	r := reqWithPrincipal(t, http.MethodPost, "/copilot/chat/completions", strings.NewReader(`{}`), "test-client")
	w := httptest.NewRecorder()
	svc.HandleCopilotPassthrough(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// X-Copilot-Account should be set
	if got := resp.Header.Get("X-Copilot-Account"); got != "test-user" {
		t.Errorf("expected X-Copilot-Account=test-user, got %q", got)
	}

	// X-Copilot-Quota-Remaining should be set
	if got := resp.Header.Get("X-Copilot-Quota-Remaining"); got != "75.0" {
		t.Errorf("expected X-Copilot-Quota-Remaining=75.0, got %q", got)
	}

	// Upstream headers should be passed through
	if got := resp.Header.Get("X-Custom-Upstream"); got != "some-value" {
		t.Errorf("expected X-Custom-Upstream=some-value, got %q", got)
	}

	// Body should be passed through
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"result":"ok"}` {
		t.Errorf("expected body {\"result\":\"ok\"}, got %q", string(body))
	}
}

// ---------- HandleCopilotModels ----------

func TestHandleCopilotModels_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("expected path /models, got %q", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	defer upstream.Close()

	svc := setupPassthroughTestEnv(t, upstream.URL)

	r := reqWithPrincipal(t, http.MethodGet, "/copilot/models", nil, "test-client")
	w := httptest.NewRecorder()

	svc.HandleCopilotModels(w, r)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	if got := resp.Header.Get("X-Copilot-Account"); got != "test-user" {
		t.Errorf("expected X-Copilot-Account=test-user, got %q", got)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "gpt-4o") {
		t.Errorf("expected body to contain gpt-4o, got %q", string(body))
	}
}
