package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const (
	requestIDKey    contextKey = "request_id"
	HeaderRequestID            = "X-Request-ID"
)

// RequestID attaches a unique request identifier to the context and response headers.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := r.Header.Get(HeaderRequestID)
			if reqID == "" {
				reqID = uuid.NewString()
			}

			ctx := context.WithValue(r.Context(), requestIDKey, reqID)
			w.Header().Set(HeaderRequestID, reqID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Recoverer ensures panics are logged and converted into HTTP 500 responses.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"panic", rec,
						"request_id", RequestIDFromContext(r.Context()),
						"method", r.Method,
						"path", r.URL.Path,
					)
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// AccessLogger logs request/response information through the provided logger.
func AccessLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()

			next.ServeHTTP(rec, r)

			duration := time.Since(start)
			logger.Info("http_request",
				"request_id", RequestIDFromContext(r.Context()),
				"remote_ip", remoteIP(r),
				"method", r.Method,
				"path", r.URL.RequestURI(),
				"status", rec.status,
				"bytes", rec.bytesWritten,
				"duration_ms", duration.Milliseconds(),
			)
		})
	}
}

// RequestIDFromContext retrieves the request ID if available.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

type responseRecorder struct {
	http.ResponseWriter
	status       int
	bytesWritten int64
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytesWritten += int64(n)
	return n, err
}

// Flush implements http.Flusher so that SSE streaming works correctly
// through the middleware chain. Without this, streamingWriter cannot
// detect the Flusher interface and SSE frames accumulate in the buffer.
func (r *responseRecorder) Flush() {
	if fl, ok := r.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter, allowing http.NewResponseController
// and similar utilities to discover interfaces on the original writer.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func remoteIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
