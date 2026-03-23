package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ycgame/llms-proxy/internal/config"
)

// UserStore manages admin users backed by a JSON file.
type UserStore struct {
	mu   sync.RWMutex
	path string
}

// NewUserStore creates a file-backed admin user store.
func NewUserStore(path string) *UserStore {
	return &UserStore{path: strings.TrimSpace(path)}
}

// Path returns current file path.
func (s *UserStore) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// SetPath updates the file path.
func (s *UserStore) SetPath(path string) {
	s.mu.Lock()
	s.path = strings.TrimSpace(path)
	s.mu.Unlock()
}

// List returns all admin users.
func (s *UserStore) List() ([]config.AdminUser, error) {
	s.mu.RLock()
	path := s.path
	s.mu.RUnlock()
	users, err := readAdminUsers(path)
	if err != nil {
		return nil, err
	}
	cloned := make([]config.AdminUser, len(users))
	copy(cloned, users)
	return cloned, nil
}

// SeedDefaultUser writes a default admin user to the store if the file is
// empty.  It is safe to call on every startup — existing users are never
// modified.
func (s *UserStore) SeedDefaultUser(user config.AdminUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := readAdminUsers(s.path)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil // already has users
	}

	data, err := json.MarshalIndent([]config.AdminUser{user}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal default admin user: %w", err)
	}
	data = append(data, '\n')
	return writeAtomicFile(s.path, data)
}

// Authenticate verifies credentials and returns the matching user.
func (s *UserStore) Authenticate(username, password string) (config.AdminUser, error) {
	users, err := s.List()
	if err != nil {
		return config.AdminUser{}, err
	}

	username = strings.TrimSpace(username)
	for _, user := range users {
		if !strings.EqualFold(strings.TrimSpace(user.Username), username) {
			continue
		}
		if user.Disabled {
			return config.AdminUser{}, errors.New("account is disabled")
		}
		ok, err := verifyPasswordHash(user.PasswordHash, password)
		if err != nil {
			return config.AdminUser{}, err
		}
		if !ok {
			return config.AdminUser{}, errors.New("invalid credentials")
		}
		if strings.TrimSpace(user.Role) == "" {
			user.Role = "admin"
		}
		return user, nil
	}

	return config.AdminUser{}, errors.New("invalid credentials")
}

func readAdminUsers(path string) ([]config.AdminUser, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("admin users file path is empty")
	}
	if err := ensureJSONListFile(path); err != nil {
		return nil, err
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read admin users file: %w", err)
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return []config.AdminUser{}, nil
	}

	var users []config.AdminUser
	if err := json.Unmarshal(content, &users); err != nil {
		return nil, fmt.Errorf("decode admin users file: %w", err)
	}

	seen := make(map[string]struct{}, len(users))
	for i := range users {
		users[i].Username = strings.TrimSpace(users[i].Username)
		if users[i].Username == "" {
			return nil, fmt.Errorf("admin_users[%d]: username must not be empty", i)
		}
		if users[i].PasswordHash == "" {
			return nil, fmt.Errorf("admin_users[%d]: password_hash must not be empty", i)
		}
		if users[i].Role == "" {
			users[i].Role = "admin"
		}
		key := strings.ToLower(users[i].Username)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate admin username %q", users[i].Username)
		}
		seen[key] = struct{}{}
	}

	return users, nil
}

func ensureJSONListFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create admin dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeAtomicFile(path, []byte("[]\n")); err != nil {
		return err
	}
	return nil
}

// UpdatePasswordHash changes the password hash for the given username.
func (s *UserStore) UpdatePasswordHash(username, newHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("username must not be empty")
	}

	users, err := readAdminUsers(s.path)
	if err != nil {
		return err
	}

	found := false
	for i := range users {
		if strings.EqualFold(strings.TrimSpace(users[i].Username), username) {
			users[i].PasswordHash = newHash
			users[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("user %q not found", username)
	}

	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal admin users: %w", err)
	}
	data = append(data, '\n')
	return writeAtomicFile(s.path, data)
}

// writeAtomicFile writes data to a temp file then renames it to the target
// path. This only requires write permission on the parent directory, not on the
// target file itself.
func writeAtomicFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", filepath.Base(path), uuid.NewString()))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// HashPasswordWithRandomSalt generates a random 16-byte salt and hashes the password.
func HashPasswordWithRandomSalt(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	return HashPassword(password, hex.EncodeToString(salt)), nil
}

// HashPassword returns a deterministic SHA-256 based password hash string.
func HashPassword(password, salt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(salt) + ":" + password))
	return fmt.Sprintf("sha256$%s$%s", strings.TrimSpace(salt), hex.EncodeToString(sum[:]))
}

func verifyPasswordHash(encoded, password string) (bool, error) {
	encoded = strings.TrimSpace(encoded)
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 {
		return false, fmt.Errorf("unsupported password hash format")
	}
	if parts[0] != "sha256" {
		return false, fmt.Errorf("unsupported password hash algorithm %q", parts[0])
	}
	actual := HashPassword(password, parts[1])
	expected := strings.ToLower(strings.TrimSpace(encoded))
	if len(actual) != len(expected) {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1, nil
}
