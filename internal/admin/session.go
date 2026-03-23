package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ycgame/llms-proxy/internal/config"
)

type sessionContextKey struct{}

// Session represents a signed admin login session.
type Session struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SessionManager stores admin sessions in memory and signs cookies.
type SessionManager struct {
	mu       sync.RWMutex
	secret   []byte
	config   config.AdminSessionConfig
	sessions map[string]*Session
	logger   *slog.Logger
}

// NewSessionManager creates an admin session manager.
func NewSessionManager(cfg config.AdminSessionConfig, logger *slog.Logger) *SessionManager {
	return &SessionManager{
		secret:   []byte(strings.TrimSpace(cfg.Secret)),
		config:   cfg,
		sessions: make(map[string]*Session),
		logger:   logger,
	}
}

// UpdateConfig refreshes cookie/session settings.
func (m *SessionManager) UpdateConfig(cfg config.AdminSessionConfig) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.secret = []byte(strings.TrimSpace(cfg.Secret))
	m.config = cfg
	m.mu.Unlock()
}

// Config returns the current session config.
func (m *SessionManager) Config() config.AdminSessionConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// CreateSession creates a new signed session cookie.
func (m *SessionManager) CreateSession(username, role string) (*Session, *http.Cookie, error) {
	if m == nil {
		return nil, nil, errors.New("session manager is nil")
	}
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(m.Config().TTLSeconds) * time.Second)
	if role == "" {
		role = "admin"
	}
	session := &Session{
		ID:        sessionID,
		Username:  strings.TrimSpace(username),
		Role:      strings.TrimSpace(role),
		CreatedAt: now,
		LastSeen:  now,
		ExpiresAt: expiresAt,
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	cookie := m.newCookie(sessionID, expiresAt)
	return session, cookie, nil
}

// CurrentSession returns the session attached to the request.
func (m *SessionManager) CurrentSession(r *http.Request) (*Session, bool) {
	if m == nil || r == nil {
		return nil, false
	}
	cookie, err := r.Cookie(m.Config().CookieName)
	if err != nil {
		return nil, false
	}
	sessionID, ok := m.verifyCookie(cookie.Value)
	if !ok {
		return nil, false
	}
	return m.sessionByID(sessionID)
}

// DestroySession removes the current session and clears the cookie.
func (m *SessionManager) DestroySession(w http.ResponseWriter, r *http.Request) {
	if m == nil {
		return
	}
	if cookie, err := r.Cookie(m.Config().CookieName); err == nil {
		if sessionID, ok := m.verifyCookie(cookie.Value); ok {
			m.mu.Lock()
			delete(m.sessions, sessionID)
			m.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     m.Config().CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   m.Config().SecureCookie,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// Middleware enforces admin session authentication.
func (m *SessionManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := m.CurrentSession(r)
		if !ok {
			m.unauthorized(w, r)
			return
		}

		if updated, ok := m.touch(session.ID); ok {
			http.SetCookie(w, m.newCookie(updated.ID, updated.ExpiresAt))
			r = r.WithContext(ContextWithSession(r.Context(), updated))
		} else {
			r = r.WithContext(ContextWithSession(r.Context(), session))
		}
		next.ServeHTTP(w, r)
	})
}

// ContextWithSession stores a session in context.
func ContextWithSession(ctx context.Context, session *Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, session)
}

// SessionFromContext extracts a session from context.
func SessionFromContext(ctx context.Context) (*Session, bool) {
	if ctx == nil {
		return nil, false
	}
	session, ok := ctx.Value(sessionContextKey{}).(*Session)
	if !ok || session == nil {
		return nil, false
	}
	return session, true
}

func (m *SessionManager) unauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/admin/api/") || strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (m *SessionManager) sessionByID(sessionID string) (*Session, bool) {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if now.After(session.ExpiresAt) {
		delete(m.sessions, sessionID)
		return nil, false
	}
	cloned := *session
	return &cloned, true
}

func (m *SessionManager) touch(sessionID string) (*Session, bool) {
	if !m.Config().SlidingExpiration {
		if session, ok := m.sessionByID(sessionID); ok {
			return session, true
		}
		return nil, false
	}

	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[sessionID]
	if !ok || now.After(session.ExpiresAt) {
		delete(m.sessions, sessionID)
		return nil, false
	}
	session.LastSeen = now
	session.ExpiresAt = now.Add(time.Duration(m.config.TTLSeconds) * time.Second)
	cloned := *session
	return &cloned, true
}

func (m *SessionManager) newCookie(sessionID string, expiresAt time.Time) *http.Cookie {
	cfg := m.Config()
	value := m.sign(sessionID)
	return &http.Cookie{
		Name:     cfg.CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cfg.SecureCookie,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		Expires:  expiresAt,
	}
}

func (m *SessionManager) sign(sessionID string) string {
	mac := hmac.New(sha256.New, m.secretBytes())
	_, _ = mac.Write([]byte(sessionID))
	return sessionID + "." + hex.EncodeToString(mac.Sum(nil))
}

func (m *SessionManager) verifyCookie(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return "", false
	}
	if parts[0] == "" || parts[1] == "" {
		return "", false
	}
	expected := m.sign(parts[0])
	if subtleConstantTimeStringCompare(expected, value) {
		return parts[0], true
	}
	return "", false
}

func (m *SessionManager) secretBytes() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]byte(nil), m.secret...)
}

func generateSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func subtleConstantTimeStringCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
