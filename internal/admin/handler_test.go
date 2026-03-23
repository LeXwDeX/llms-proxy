package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/config"
	"github.com/ycgame/azure-proxy/internal/proxy"
	"github.com/ycgame/azure-proxy/internal/usage"
)

func TestHandlerUIEntry(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), clients)
	writeConfigFile(t, configPath, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, nil, nil, logger)

	for _, route := range []string{"/", "/ui"} {
		t.Run(route, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com"+route, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("ui entry expected 200, got %d", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Fatalf("expected text/html content-type, got %q", ct)
			}

			body := rec.Body.String()
			if !strings.Contains(body, "客户端管理") || !strings.Contains(body, "消费统计") {
				t.Fatalf("ui page missing expected tab labels")
			}
			if !strings.Contains(body, "/admin/data/clients") || !strings.Contains(body, "/admin/data/usage/aggregate") {
				t.Fatalf("ui page missing expected admin api endpoints")
			}
		})
	}
}

func TestHandlerUIEntryWhenMountedUnderAuth(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), clients)
	writeConfigFile(t, configPath, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	adminHandler := NewHandler(manager, store, service, nil, nil, logger)
	protected := chi.NewRouter()
	protected.Use(auth.Middleware(store, logger))
	protected.Mount("/admin", adminHandler)

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/admin/ui", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	withAuth := httptest.NewRequest(http.MethodGet, "http://example.com/admin/ui", nil)
	withAuth.Header.Set("Authorization", "Bearer k1")
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, withAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", rec.Code)
	}
}

func TestHandlerHealthz(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 2, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), clients)
	writeConfigFile(t, configPath, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, nil, nil, logger)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		Status      string               `json:"status"`
		Targets     []proxy.TargetStatus `json:"targets"`
		TargetCount int                  `json:"target_count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status == "" {
		t.Fatal("expected status field to be populated")
	}
	if resp.TargetCount != 2 {
		t.Fatalf("expected target_count 2, got %d", resp.TargetCount)
	}
	if len(resp.Targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(resp.Targets))
	}
}

func TestHandlerMetrics(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), clients)
	writeConfigFile(t, configPath, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, nil, nil, logger)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		UptimeSeconds float64 `json:"uptime_seconds"`
		Targets       int     `json:"targets"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.UptimeSeconds < 0 {
		t.Fatalf("expected non-negative uptime, got %f", resp.UptimeSeconds)
	}
	if resp.Targets != 1 {
		t.Fatalf("expected targets 1, got %d", resp.Targets)
	}
}

func TestHandlerReloadConfig(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	initial := testConfig(tempDir, 1, []string{"k1"})
	initialClients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), initialClients)
	writeConfigFile(t, configPath, initial)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(initialClients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, nil, nil, logger)

	updated := testConfig(tempDir, 2, []string{"k2"})
	updatedClients := testClients([]string{"k2"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), updatedClients)
	writeConfigFile(t, configPath, updated)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status  string `json:"status"`
		Targets int    `json:"targets"`
		Clients int    `json:"clients"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", resp.Status)
	}
	if resp.Targets != 2 || resp.Clients != 1 {
		t.Fatalf("unexpected counts: targets=%d clients=%d", resp.Targets, resp.Clients)
	}

	current, err := manager.Current()
	if err != nil {
		t.Fatalf("expected manager.Current to succeed, got %v", err)
	}
	if len(current.AzureTargets) != 2 {
		t.Fatalf("expected manager cache to be updated to 2 targets, got %d", len(current.AzureTargets))
	}

	if _, ok := store.Authenticate("k2"); !ok {
		t.Fatal("expected auth store to contain updated client key k2")
	}
	if _, ok := store.Authenticate("k1"); ok {
		t.Fatal("expected auth store to drop old client key k1")
	}
}

func TestHandlerReloadConfigRejectsInvalidProxyConfig(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	initial := testConfig(tempDir, 1, []string{"k1"})
	initialClients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), initialClients)
	writeConfigFile(t, configPath, initial)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	manager.Replace(initial)

	store := auth.NewStore()
	if err := store.LoadFromConfig(initialClients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}

	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, nil, nil, logger)

	invalid := testConfig(tempDir, 1, []string{"k2"})
	invalidClients := testClients([]string{"k2"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), invalidClients)
	invalid.AzureTargets[0].Endpoint = "not-a-valid-url"
	writeConfigFile(t, configPath, invalid)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/config/reload", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	if _, ok := store.Authenticate("k1"); !ok {
		t.Fatal("expected original key k1 to remain after rejected reload")
	}
	if _, ok := store.Authenticate("k2"); ok {
		t.Fatal("expected new key k2 to be rejected with invalid proxy config")
	}

	current, err := manager.Current()
	if err != nil {
		t.Fatalf("expected manager.Current to return cached config, got %v", err)
	}
	if len(current.AzureTargets) != 1 || current.AzureTargets[0].Endpoint != initial.AzureTargets[0].Endpoint {
		t.Fatalf("expected manager cache to remain unchanged, got %#v", current.AzureTargets)
	}
}

func TestHandlerDataClientsCRUD(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), clients)
	writeConfigFile(t, configPath, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, nil, nil, logger)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/clients", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list clients expected 200, got %d", rec.Code)
	}

	body := bytes.NewBufferString(`{"name":"client-2","access_key":"k2","allowed_targets":[]}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/data/clients", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k2"); !ok {
		t.Fatalf("expected auth store updated with k2")
	}

	body = bytes.NewBufferString(`{"name":"client-2","access_key":"k3","allowed_targets":[]}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/clients/client-2", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("update client expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k3"); !ok {
		t.Fatalf("expected auth store updated with k3")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "http://example.com/data/clients/client-1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete client expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Authenticate("k1"); ok {
		t.Fatalf("expected client-1 removed from auth store")
	}
}

func TestHandlerUsageSummary(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfg := testConfig(tempDir, 1, []string{"k1"})
	clients := testClients([]string{"k1"})
	writeClientsFile(t, filepath.Join(tempDir, "clients.json"), clients)
	writeConfigFile(t, configPath, cfg)

	usageStore := usage.NewStore(filepath.Join(tempDir, "usage_events.jsonl"))
	now := time.Now().UTC()
	if err := usageStore.Record(usage.Event{Timestamp: now.Add(-10 * time.Minute), ClientName: "client-1", Model: "gpt-4o", InputTokens: 100, OutputTokens: 50}); err != nil {
		t.Fatalf("record usage event: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, nil, nil, logger)

	body := bytes.NewBufferString(`{"input_per_1k_tokens":0.01,"output_per_1k_tokens":0.02,"cached_input_per_1k_tokens":0}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "http://example.com/data/model-costs/gpt-4o", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert model cost expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.com/data/usage/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		LastHour struct {
			Requests int64 `json:"requests"`
		} `json:"last_hour"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode summary response: %v", err)
	}
	if payload.LastHour.Requests == 0 {
		t.Fatalf("expected summary last_hour requests > 0")
	}
}

func writeConfigFile(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
}

func testConfig(tempDir string, targetCount int, clientKeys []string) *config.Config {
	_ = clientKeys
	targets := make([]config.AzureTarget, 0, targetCount)
	for i := 0; i < targetCount; i++ {
		targets = append(targets, config.AzureTarget{
			Name:               fmt.Sprintf("target-%d", i+1),
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			AzureAPIKey:        "key",
			AllowedModels:      []string{"gpt-4o"},
		})
	}

	return &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		AzureTargets: targets,
		DataFiles: config.DataFiles{
			ClientsFile:     filepath.Join(tempDir, "clients.json"),
			ModelCostsFile:  filepath.Join(tempDir, "model_costs.json"),
			UsageEventsFile: filepath.Join(tempDir, "usage_events.jsonl"),
			AdminUsersFile:  filepath.Join(tempDir, "admin_users.json"),
			AdminAuditFile:  filepath.Join(tempDir, "admin_audit.jsonl"),
		},
		AdminSession: config.AdminSessionConfig{
			CookieName: "admin_sid",
			Secret:     "test-secret",
			TTLSeconds: 3600,
		},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: filepath.Join(tempDir, "logs", "access.log"),
			ErrorLog:  filepath.Join(tempDir, "logs", "error.log"),
		},
	}
}

func testClients(clientKeys []string) []config.Client {
	clients := make([]config.Client, 0, len(clientKeys))
	for i, key := range clientKeys {
		clients = append(clients, config.Client{
			Name:      fmt.Sprintf("client-%d", i+1),
			AccessKey: key,
		})
	}
	return clients
}

func writeClientsFile(t *testing.T, path string, clients []config.Client) {
	t.Helper()
	payload, err := json.Marshal(clients)
	if err != nil {
		t.Fatalf("failed to marshal clients: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("failed to write clients file: %v", err)
	}
}
