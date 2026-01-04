package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/config"
	"github.com/ycgame/azure-proxy/internal/proxy"
)

func TestHandlerHealthz(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	cfg := testConfig(tempDir, 2, []string{"k1"})
	writeConfigFile(t, configPath, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, logger)
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
	writeConfigFile(t, configPath, cfg)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(cfg, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, logger)
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
	writeConfigFile(t, configPath, initial)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))
	manager := config.NewManager(configPath)
	store := auth.NewStore()
	if err := store.LoadFromConfig(initial.Clients); err != nil {
		t.Fatalf("failed to init auth store: %v", err)
	}
	service, err := proxy.NewService(initial, logger)
	if err != nil {
		t.Fatalf("failed to init proxy service: %v", err)
	}

	h := NewHandler(manager, store, service, logger)

	updated := testConfig(tempDir, 2, []string{"k2"})
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
	targets := make([]config.AzureTarget, 0, targetCount)
	for i := 0; i < targetCount; i++ {
		targets = append(targets, config.AzureTarget{
			Name:               fmt.Sprintf("target-%d", i+1),
			Endpoint:           "https://example.com",
			ResourcePathPrefix: "/openai",
			AzureAPIKey:        "key",
			DefaultAPIVersion:  "2025-04-01-preview",
			AllowedModels:      []string{"gpt-4o"},
		})
	}

	clients := make([]config.Client, 0, len(clientKeys))
	for i, key := range clientKeys {
		clients = append(clients, config.Client{
			Name:      fmt.Sprintf("client-%d", i+1),
			AccessKey: key,
		})
	}

	return &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		AzureTargets: targets,
		Clients:      clients,
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: filepath.Join(tempDir, "logs", "access.log"),
			ErrorLog:  filepath.Join(tempDir, "logs", "error.log"),
		},
	}
}
