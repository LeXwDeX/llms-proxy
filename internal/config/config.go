package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config is the top-level configuration model matching config/config.json.
type Config struct {
	Server       ServerConfig       `json:"server"`
	AzureTargets []AzureTarget      `json:"azure_targets"`
	DataFiles    DataFiles          `json:"data_files"`
	AdminSession AdminSessionConfig `json:"admin_session"`
	Logging      LoggingConfig      `json:"logging"`
}

// DataFiles contains paths to file-backed NoSQL data.
type DataFiles struct {
	ClientsFile     string `json:"clients_file"`
	ModelCostsFile  string `json:"model_costs_file"`
	UsageEventsFile string `json:"usage_events_file"`
	AdminUsersFile  string `json:"admin_users_file"`
	AdminAuditFile  string `json:"admin_audit_file"`
}

// AdminSessionConfig controls the admin login session.
type AdminSessionConfig struct {
	CookieName        string `json:"cookie_name"`
	Secret            string `json:"secret"`
	TTLSeconds        int    `json:"ttl_seconds"`
	SlidingExpiration bool   `json:"sliding_expiration"`
	SecureCookie      bool   `json:"secure_cookie"`
}

// AdminUser describes one admin account.
type AdminUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// ServerConfig controls the HTTP server behaviour.
type ServerConfig struct {
	Bind                  string `json:"bind"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
}

// AzureTarget represents one Azure OpenAI endpoint.
type AzureTarget struct {
	Name               string   `json:"name"`
	Endpoint           string   `json:"endpoint"`
	ResourcePathPrefix string   `json:"resource_path_prefix"`
	AzureAPIKey        string   `json:"azure_api_key"`
	AllowBearer        bool     `json:"allow_bearer_passthrough"`
	AllowedModels      []string `json:"allowed_models"`
}

// Client describes a consumer and its access rights.
type Client struct {
	Name           string   `json:"name"`
	AccessKey      string   `json:"access_key"`
	AllowedTargets []string `json:"allowed_targets"`
}

// LoggingConfig contains logging level and file paths.
type LoggingConfig struct {
	Level     string `json:"level"`
	AccessLog string `json:"access_log"`
	ErrorLog  string `json:"error_log"`
}

// Manager caches configuration content loaded from disk.
type Manager struct {
	path string

	mu    sync.RWMutex
	cache *Config
}

// NewManager creates a Manager bound to a config file path.
func NewManager(path string) *Manager {
	return &Manager{path: path}
}

// Path returns the configured config file path.
func (m *Manager) Path() string {
	return m.path
}

// Current returns the cached configuration. If not yet loaded it attempts to load it.
func (m *Manager) Current() (*Config, error) {
	m.mu.RLock()
	if m.cache != nil {
		cfg := m.cache.Clone()
		m.mu.RUnlock()
		return cfg, nil
	}
	m.mu.RUnlock()

	return m.Reload()
}

// Reload forces reading the configuration file from disk.
func (m *Manager) Reload() (*Config, error) {
	cfg, err := Load(m.path)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.cache = cfg
	m.mu.Unlock()
	return cfg.Clone(), nil
}

// Replace updates the cached configuration without reading from disk.
func (m *Manager) Replace(cfg *Config) {
	if m == nil {
		return
	}

	m.mu.Lock()
	if cfg == nil {
		m.cache = nil
	} else {
		m.cache = cfg.Clone()
	}
	m.mu.Unlock()
}

// Load reads and validates the config file from path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	cfg, err := parse(content)
	if err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %s: %w", path, err)
	}

	resolveDataFilePaths(cfg, filepath.Dir(path))

	return cfg, nil
}

func parse(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate performs semantic checks on the configuration.
func (c *Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.Server.Bind) == "" {
		problems = append(problems, "server.bind must not be empty")
	}

	if c.Server.RequestTimeoutSeconds <= 0 {
		problems = append(problems, "server.request_timeout_seconds must be greater than zero")
	}

	if len(c.AzureTargets) == 0 {
		problems = append(problems, "azure_targets must contain at least one entry")
	}

	for i, target := range c.AzureTargets {
		prefix := fmt.Sprintf("azure_targets[%d]", i)
		if strings.TrimSpace(target.Name) == "" {
			problems = append(problems, prefix+" name must not be empty")
		}
		if strings.TrimSpace(target.Endpoint) == "" {
			problems = append(problems, prefix+" endpoint must not be empty")
		}
		if strings.TrimSpace(target.ResourcePathPrefix) == "" {
			problems = append(problems, prefix+" resource_path_prefix must not be empty")
		}
		if strings.TrimSpace(target.AzureAPIKey) == "" && !target.AllowBearer {
			problems = append(problems, prefix+" azure_api_key must not be empty when allow_bearer_passthrough is false")
		}
		for j, m := range target.AllowedModels {
			if strings.TrimSpace(m) == "" {
				problems = append(problems, fmt.Sprintf("%s allowed_models[%d] must not be empty", prefix, j))
			}
		}
	}

	if strings.TrimSpace(c.DataFiles.ClientsFile) == "" {
		problems = append(problems, "data_files.clients_file must not be empty")
	}
	if strings.TrimSpace(c.DataFiles.ModelCostsFile) == "" {
		problems = append(problems, "data_files.model_costs_file must not be empty")
	}
	if strings.TrimSpace(c.DataFiles.UsageEventsFile) == "" {
		problems = append(problems, "data_files.usage_events_file must not be empty")
	}
	if strings.TrimSpace(c.DataFiles.AdminUsersFile) == "" {
		problems = append(problems, "data_files.admin_users_file must not be empty")
	}
	if strings.TrimSpace(c.DataFiles.AdminAuditFile) == "" {
		problems = append(problems, "data_files.admin_audit_file must not be empty")
	}

	if strings.TrimSpace(c.AdminSession.CookieName) == "" {
		problems = append(problems, "admin_session.cookie_name must not be empty")
	}
	if strings.TrimSpace(c.AdminSession.Secret) == "" {
		problems = append(problems, "admin_session.secret must not be empty")
	}
	if c.AdminSession.TTLSeconds <= 0 {
		problems = append(problems, "admin_session.ttl_seconds must be greater than zero")
	}

	if strings.TrimSpace(c.Logging.Level) == "" {
		problems = append(problems, "logging.level must not be empty")
	}
	if strings.TrimSpace(c.Logging.AccessLog) == "" {
		problems = append(problems, "logging.access_log must not be empty")
	}
	if strings.TrimSpace(c.Logging.ErrorLog) == "" {
		problems = append(problems, "logging.error_log must not be empty")
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// Clone returns a deep copy of the Config to avoid accidental mutation.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}

	clone := *c
	if len(c.AzureTargets) > 0 {
		clone.AzureTargets = make([]AzureTarget, len(c.AzureTargets))
		for i := range c.AzureTargets {
			clone.AzureTargets[i] = c.AzureTargets[i]
			if len(c.AzureTargets[i].AllowedModels) > 0 {
				clone.AzureTargets[i].AllowedModels = append([]string(nil), c.AzureTargets[i].AllowedModels...)
			}
		}
	}

	return &clone
}

func resolveDataFilePaths(cfg *Config, baseDir string) {
	if cfg == nil {
		return
	}
	cfg.DataFiles.ClientsFile = resolvePath(baseDir, cfg.DataFiles.ClientsFile)
	cfg.DataFiles.ModelCostsFile = resolvePath(baseDir, cfg.DataFiles.ModelCostsFile)
	cfg.DataFiles.UsageEventsFile = resolvePath(baseDir, cfg.DataFiles.UsageEventsFile)
	cfg.DataFiles.AdminUsersFile = resolvePath(baseDir, cfg.DataFiles.AdminUsersFile)
	cfg.DataFiles.AdminAuditFile = resolvePath(baseDir, cfg.DataFiles.AdminAuditFile)
}

func resolvePath(baseDir, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if strings.TrimSpace(baseDir) == "" {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(baseDir, path))
}
