package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ycgame/llms-proxy/internal/config"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// AuthenticateUser verifies credentials against the nosql UserStore.
func AuthenticateUser(store *nosql.UserStore, username, password string) (config.AdminUser, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return config.AdminUser{}, errors.New("invalid credentials")
	}
	user, err := store.Get(username)
	if err != nil {
		return config.AdminUser{}, errors.New("invalid credentials")
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
