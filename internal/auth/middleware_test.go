package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ycgame/azure-proxy/internal/config"
)

func TestMiddlewareAcceptsAzureStyleAuth(t *testing.T) {
	store := NewStore()
	if err := store.LoadFromConfig([]config.Client{
		{Name: "bearer", AccessKey: "token", AllowedTargets: nil},
		{Name: "apikey", AccessKey: "apikey-value", AllowedTargets: nil},
	}); err != nil {
		t.Fatalf("load config: %v", err)
	}

	tests := []struct {
		name       string
		headerKey  string
		headerVal  string
		queryValue string
		wantStatus int
	}{
		{name: "bearer ok", headerKey: "Authorization", headerVal: "Bearer token", wantStatus: http.StatusOK},
		{name: "api-key header ok", headerKey: "api-key", headerVal: "apikey-value", wantStatus: http.StatusOK},
		{name: "api-key query ok", queryValue: "apikey-value", wantStatus: http.StatusOK},
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "bad bearer scheme", headerKey: "Authorization", headerVal: "Basic token", wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
			handler := Middleware(store, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			url := "/"
			if tt.queryValue != "" {
				url = "/?api-key=" + tt.queryValue
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tt.headerKey != "" {
				req.Header.Set(tt.headerKey, tt.headerVal)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, rr.Code)
			}
		})
	}
}
