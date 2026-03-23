package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	appmiddleware "github.com/ycgame/llms-proxy/internal/middleware"
)

type contextKey string

const (
	principalContextKey contextKey = "auth_principal"
)

// WithPrincipal stores the authenticated principal in the context.
func WithPrincipal(ctx context.Context, principal *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey, principal)
}

// PrincipalFromContext returns the authenticated principal.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	if ctx == nil {
		return nil, false
	}
	principal, ok := ctx.Value(principalContextKey).(*Principal)
	return principal, ok && principal != nil
}

// Middleware returns an HTTP middleware enforcing Authorization: Bearer headers.
func Middleware(store *Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := authenticate(store, r)
			if err != nil {
				logger.Warn("authentication failed",
					"request_id", appmiddleware.RequestIDFromContext(r.Context()),
					"remote_ip", r.RemoteAddr,
					"reason", err.Error(),
				)
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := WithPrincipal(r.Context(), principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func authenticate(store *Store, r *http.Request) (*Principal, error) {
	token := extractAccessKey(r)
	if token == "" {
		return nil, ErrMissingAuthorization
	}

	principal, ok := store.Authenticate(token)
	if !ok {
		return nil, ErrInvalidCredential
	}
	return principal, nil
}

func extractAccessKey(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token := strings.TrimSpace(parts[1])
			if token != "" {
				return token
			}
		}
	}
	if apiKey := strings.TrimSpace(r.Header.Get("api-key")); apiKey != "" {
		return apiKey
	}
	if apiKey := strings.TrimSpace(r.Header.Get("x-api-key")); apiKey != "" {
		return apiKey
	}
	if r.URL != nil {
		if qKey := extractAPIKeyFromQuery(r.URL.RawQuery); qKey != "" {
			return qKey
		}
	}
	return ""
}

func extractAPIKeyFromQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	pairs := strings.Split(rawQuery, "&")
	for _, pair := range pairs {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, val := strings.TrimSpace(kv[0]), kv[1]
		if !strings.EqualFold(key, "api-key") {
			continue
		}
		decoded, err := url.PathUnescape(strings.TrimSpace(val))
		if err != nil {
			continue
		}
		if decoded != "" {
			return decoded
		}
	}
	return ""
}

var (
	ErrMissingAuthorization = errors.New("authorization header missing or malformed")
	ErrInvalidCredential    = errors.New("invalid access key")
)
