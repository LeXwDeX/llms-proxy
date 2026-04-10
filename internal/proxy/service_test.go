package proxy

import (
	"bytes"
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

func TestServiceRetriesOnUpstream503(t *testing.T) {
	restore := setUpstream503RetryConfig(2, 2*time.Millisecond, 0)
	defer restore()

	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"busy"}`))
			return
		}
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

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 upstream attempts, got %d", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries != 2 {
		t.Fatalf("expected 2 retries for upstream 503, got %d", metrics.TotalRetries)
	}
}

func TestServiceReturns503AfterExhaustingUpstream503Retries(t *testing.T) {
	restore := setUpstream503RetryConfig(2, 2*time.Millisecond, 0)
	defer restore()

	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
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
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 upstream attempts, got %d", got)
	}

	metrics := service.MetricsSnapshot()
	if metrics.TotalRetries != 2 {
		t.Fatalf("expected 2 retries for upstream 503, got %d", metrics.TotalRetries)
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
	if seenVersion != "" {
		t.Fatalf("expected api-version to be stripped, got %q", seenVersion)
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

func setUpstream503RetryConfig(maxRetries int, delay time.Duration, jitter time.Duration) func() {
	oldMaxRetries := upstream503MaxRetries
	oldDelay := upstream503RetryDelay
	oldJitter := upstream503RetryJitter

	upstream503MaxRetries = maxRetries
	upstream503RetryDelay = delay
	upstream503RetryJitter = jitter

	return func() {
		upstream503MaxRetries = oldMaxRetries
		upstream503RetryDelay = oldDelay
		upstream503RetryJitter = oldJitter
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
