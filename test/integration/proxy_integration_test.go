//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ycgame/azure-proxy/internal/admin"
	"github.com/ycgame/azure-proxy/internal/auth"
	"github.com/ycgame/azure-proxy/internal/config"
	"github.com/ycgame/azure-proxy/internal/logging"
	"github.com/ycgame/azure-proxy/internal/proxy"
)

func TestEndToEndAdminAndProxyFlow(t *testing.T) {
	success := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "integration-success"})
	}))
	defer success.Close()

	tempDir := t.TempDir()
	cfg := &config.Config{
		Server: config.ServerConfig{
			Bind:                  "127.0.0.1:0",
			RequestTimeoutSeconds: 5,
		},
		AzureTargets: []config.AzureTarget{
			{
				Name:               "primary",
				Endpoint:           "http://127.0.0.1:1",
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
			Name:      "integration",
			AccessKey: "integration-token",
		}},
		Logging: config.LoggingConfig{
			Level:     "info",
			AccessLog: filepath.Join(tempDir, "access.log"),
			ErrorLog:  filepath.Join(tempDir, "error.log"),
		},
	}

	logManager, err := logging.Setup(logging.Config{
		Level:         cfg.Logging.Level,
		AccessLogPath: cfg.Logging.AccessLog,
		ErrorLogPath:  cfg.Logging.ErrorLog,
	})
	if err != nil {
		t.Fatalf("logging setup: %v", err)
	}
	defer logManager.Close()

	appLogger := logManager.App()

	proxyService, err := proxy.NewService(cfg, appLogger)
	if err != nil {
		t.Fatalf("proxy service: %v", err)
	}

	store := auth.NewStore()
	if err := store.LoadFromConfig(cfg.Clients); err != nil {
		t.Fatalf("load clients: %v", err)
	}
	principal, ok := store.Authenticate("integration-token")
	if !ok {
		t.Fatalf("expected principal for integration token")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewBufferString(`{"prompt":"ping"}`))
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	rr := httptest.NewRecorder()

	proxyService.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}
	if resp.Header.Get("X-Azure-Target") != "secondary" {
		t.Fatalf("expected fallback to secondary target, header=%q", resp.Header.Get("X-Azure-Target"))
	}

	manager := config.NewManager("testdata/config.json")
	manager.Replace(cfg)

	adminHandler := admin.NewHandler(manager, store, proxyService, appLogger)

	t.Run("health endpoint", func(t *testing.T) {
		rec := httptest.NewRecorder()
		adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}
		var payload struct {
			Targets []proxy.TargetStatus `json:"targets"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		if len(payload.Targets) != 2 {
			t.Fatalf("expected 2 targets, got %d", len(payload.Targets))
		}
	})

	t.Run("metrics endpoint", func(t *testing.T) {
		rec := httptest.NewRecorder()
		adminHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", rec.Code)
		}
		var payload struct {
			Requests struct {
				Total   int64 `json:"total"`
				Success int64 `json:"success"`
			} `json:"requests"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode metrics: %v", err)
		}
		if payload.Requests.Total != 1 || payload.Requests.Success != 1 {
			t.Fatalf("unexpected request counters: %+v", payload.Requests)
		}
	})
}
