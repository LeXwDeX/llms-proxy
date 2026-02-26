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
	"strings"
	"testing"
	"time"

	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/config"
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				AzureAPIKey:        "primary-key",
			},
			{
				Name:               "secondary",
				Endpoint:           success.URL,
				ResourcePathPrefix: "/",
				AzureAPIKey:        "secondary-key",
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	service.httpClient = &http.Client{
		Transport: &failingTransport{successHost: successURL.Host},
	}
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "primary",
				Endpoint:           "http://primary.local",
				ResourcePathPrefix: "/",
				AzureAPIKey:        "key",
			},
			{
				Name:               "secondary",
				Endpoint:           "http://secondary.local",
				ResourcePathPrefix: "/",
				AzureAPIKey:        "key2",
			},
		},
		Clients: []config.Client{{
			Name:           "team-alpha",
			AccessKey:      "alpha",
			AllowedTargets: []string{"primary"},
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "slow",
				Endpoint:           slow.URL,
				ResourcePathPrefix: "/",
				AzureAPIKey:        "key",
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	service.setRequestTimeout(50 * time.Millisecond)
	service.quietPeriod = 10 * time.Millisecond

	store := auth.NewStore()
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "bearer",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/",
				AllowBearer:        true,
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "restricted",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key",
				AllowedModels:      []string{"gpt-4o"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "primary",
				Endpoint:           upstream.URL,
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key",
				AllowedModels:      []string{"gpt-4o"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{{
			Name:               "primary",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			AzureAPIKey:        "key",
			AllowedModels:      []string{"gpt-5.2"},
		}},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{{
			Name:               "primary",
			Endpoint:           upstream.URL,
			ResourcePathPrefix: "/openai",
			AzureAPIKey:        "key",
			AllowedModels:      []string{"gpt-5.2"},
		}},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "t1",
				Endpoint:           target1.URL,
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
			{
				Name:               "t2",
				Endpoint:           target2.URL,
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key2",
				AllowedModels:      []string{"gpt-5.1"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "t1",
				Endpoint:           s1.URL,
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key1",
				AllowedModels:      []string{"gpt-5.1"},
			},
			{
				Name:               "t2",
				Endpoint:           s2.URL,
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key2",
				AllowedModels:      []string{"gpt-5.1"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("token")
	if !ok {
		t.Fatal("expected principal")
	}

	for i := 0; i < 6; i++ {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key1",
				AllowedModels:      []string{"gpt-4o", "gpt-5.1"},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key2",
				AllowedModels:      []string{"gpt-5.1", "gpt-4o-mini"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key2",
				AllowedModels:      []string{"gpt-5.2"},
			},
		},
		Clients: []config.Client{{
			Name:           "tester",
			AccessKey:      "token",
			AllowedTargets: []string{"t1"},
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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
		AzureTargets: []config.AzureTarget{
			{
				Name:               "t1",
				Endpoint:           "http://example.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key1",
				AllowedModels:      []string{"gpt-4o"},
			},
			{
				Name:               "t2",
				Endpoint:           "http://example2.com",
				ResourcePathPrefix: "/openai",
				AzureAPIKey:        "key2",
				AllowedModels:      []string{"gpt-5.2"},
			},
		},
		Clients: []config.Client{{
			Name:      "tester",
			AccessKey: "token",
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
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
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

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
