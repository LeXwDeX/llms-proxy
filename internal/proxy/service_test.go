package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
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
		return nil, context.DeadlineExceeded
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

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
