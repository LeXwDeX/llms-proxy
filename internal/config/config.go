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

const (
	EndpointTypeAzureOpenAI  = "azure_openai"
	EndpointTypeOpenAI       = "openai"
	EndpointTypeClaude       = "claude"
	EndpointTypeGemini       = "gemini"
	EndpointTypeWangsuOpenAI          = "wangsu_openai"
	EndpointTypeWangsuClaude          = "wangsu_claude"
	EndpointTypeWangsuGemini          = "wangsu_gemini"
	EndpointTypeWangsuOpenAIImage     = "wangsu_openai_image"      // 网宿文生图（独立终态 URL）
	EndpointTypeWangsuOpenAIImageEdit = "wangsu_openai_image_edit" // 网宿图编辑（独立终态 URL）
	EndpointTypeCopilot               = "copilot"
	EndpointTypeDeepSeek              = "deepseek" // DeepSeek 官方（OpenAI 兼容 + Anthropic 兼容双格式，按路径自动识别）
	EndpointTypeBailian               = "bailian"           // 百炼 Token Plan（OpenAI + Anthropic 双协议，按路径自动识别）
)

// ValidEndpointTypes lists all supported endpoint types.
//
// 从 endpoint_type.go 的 endpointTypes 元数据派生，保持单一信息源。
// 新增类型请在 endpoint_type.go 的 endpointTypes 切片中追加，无需修改此处。
var ValidEndpointTypes = func() []string {
	metas := AllEndpointTypeMetas()
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Code)
	}
	return out
}()

// NormalizeEndpointType returns a canonical endpoint type; empty defaults to azure_openai.
func NormalizeEndpointType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return EndpointTypeAzureOpenAI
	}
	return t
}

// IsValidEndpointType reports whether t is a recognised endpoint type.
func IsValidEndpointType(t string) bool {
	_, ok := EndpointTypeMetaOf(t)
	return ok
}

// DataStore configures the bbolt embedded database.
type DataStore struct {
	DBPath string `json:"db_path"`
}

// Config is the top-level configuration model matching config/config.json.
type Config struct {
	Server       ServerConfig       `json:"server"`
	Targets      []Target           `json:"targets"`
	DataStore    DataStore          `json:"data_store"`
	DataFiles    DataFiles          `json:"data_files,omitempty"` // legacy, used for migration only
	AdminSession AdminSessionConfig `json:"admin_session"`
	Logging      LoggingConfig      `json:"logging"`
	TraceStore   TraceStoreConfig   `json:"trace_store,omitempty"`
}

// TraceStoreConfig controls the DEBUG mode trace store.
// 仅当 Enabled=true 时启用，生产环境应保持 false 以避免性能开销。
type TraceStoreConfig struct {
	Enabled        bool   `json:"enabled"`          // 是否启用（仅 DEBUG 时开启）
	RingBufferSize int    `json:"ring_buffer_size"` // 内存 Ring Buffer 容量（条数）
	MaxBodySize    int    `json:"max_body_size"`    // 单个 body 最大字节数
	DiskPath       string `json:"disk_path"`        // 磁盘存储路径
	DiskMaxSizeMB  int    `json:"disk_max_size_mb"` // 单个日志文件最大 MB
	DiskMaxBackups int    `json:"disk_max_backups"` // 保留的旧日志文件数
	DiskTTLHours   int    `json:"disk_ttl_hours"`   // 磁盘记录 TTL（小时）
	ChannelBuffer  int    `json:"channel_buffer"`   // 异步写入 channel buffer
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

// Target represents one upstream endpoint (Azure OpenAI, OpenAI, Claude, Gemini, or Wangsu variants).
type Target struct {
	Name               string             `json:"name"`
	EndpointType       string             `json:"endpoint_type,omitempty"` // azure_openai | openai | claude | gemini | wangsu_openai | wangsu_claude | wangsu_gemini | copilot; default azure_openai
	Endpoint           string             `json:"endpoint"`
	ResourcePathPrefix string             `json:"resource_path_prefix"`
	APIKey             string             `json:"api_key"`
	APIKeys            []string           `json:"api_keys,omitempty"`             // 额外 key 池（与 api_key 合并为有序池）
	KeyResetTime       string             `json:"key_reset_time,omitempty"`       // 额度重置时间点（CST），格式 "2006-01-02" 或 "2006-01-02 15:04" 或 "monthly:23"（每月23号）
	ProviderClass      string             `json:"provider_class,omitempty"`       // subscription | pay_as_you_go; 影响限流/超额/额度耗尽的处理策略
	AllowBearer        bool               `json:"allow_bearer_passthrough"`
	AuthMode           string             `json:"auth_mode,omitempty"` // "bearer" | "" (default: x-api-key for claude types)
	AllowedModels      []string           `json:"allowed_models"`
	SSEAutoAggregate   *bool              `json:"sse_auto_aggregate,omitempty"`   // nil defaults to true
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

// LoadOrInit loads the config file, creating a default one when missing.
// The second return value is true when a new default config was generated.
func (m *Manager) LoadOrInit() (*Config, bool, error) {
	cfg, created, err := LoadOrCreate(m.path)
	if err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	m.cache = cfg
	m.mu.Unlock()
	return cfg.Clone(), created, nil
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

// DefaultConfig returns a minimal valid configuration suitable for first-time
// startup.  The system can boot with zero upstream targets; users add targets
// later via the admin UI.
//
// Paths use the container-standard absolute directories that match the
// Dockerfile VOLUME declarations so that the auto-generated config works
// out of the box in Docker / QNAP environments.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Bind:                  "0.0.0.0:8080",
			RequestTimeoutSeconds: 1800,
		},
		Targets: []Target{},
		DataStore: DataStore{
			DBPath: "/var/lib/llms-proxy/llms-proxy.db",
		},
		AdminSession: AdminSessionConfig{
			CookieName:        "llms_proxy_admin_session",
			Secret:            "change-me-on-first-login",
			TTLSeconds:        86400,
			SlidingExpiration: true,
			SecureCookie:      false,
		},
		Logging: LoggingConfig{
			Level:     "info",
			AccessLog: "/var/log/llms-proxy/access.log",
			ErrorLog:  "/var/log/llms-proxy/error.log",
		},
	}
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

// LoadOrCreate tries to load an existing config file.  When the file does not
// exist it writes a default configuration to disk and returns it.  The second
// return value is true when a new file was created.
func LoadOrCreate(path string) (*Config, bool, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	// File does not exist — generate default config and persist it.
	cfg = DefaultConfig()
	data, marshalErr := json.MarshalIndent(cfg, "", "  ")
	if marshalErr != nil {
		return nil, false, fmt.Errorf("config: marshal default: %w", marshalErr)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return nil, false, fmt.Errorf("config: create dir %s: %w", dir, mkErr)
		}
	}
	if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
		return nil, false, fmt.Errorf("config: write default %s: %w", path, writeErr)
	}

	resolveDataFilePaths(cfg, filepath.Dir(path))
	return cfg, true, nil
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

	for i, target := range c.Targets {
		prefix := fmt.Sprintf("targets[%d]", i)

		// Normalise and validate endpoint_type.
		epType := NormalizeEndpointType(target.EndpointType)
		if !IsValidEndpointType(epType) {
			problems = append(problems, fmt.Sprintf("%s endpoint_type %q is not valid; must be one of: %s",
				prefix, target.EndpointType, strings.Join(ValidEndpointTypes, ", ")))
		}

		if strings.TrimSpace(target.Name) == "" {
			problems = append(problems, prefix+" name must not be empty")
		}
		if strings.TrimSpace(target.Endpoint) == "" {
			problems = append(problems, prefix+" endpoint must not be empty")
		}
		// resource_path_prefix is required only for azure_openai targets.
		if epType == EndpointTypeAzureOpenAI && strings.TrimSpace(target.ResourcePathPrefix) == "" {
			problems = append(problems, prefix+" resource_path_prefix must not be empty for azure_openai targets")
		}
		hasAnyKey := strings.TrimSpace(target.APIKey) != ""
		if !hasAnyKey {
			for _, k := range target.APIKeys {
				if strings.TrimSpace(k) != "" {
					hasAnyKey = true
					break
				}
			}
		}
		if !hasAnyKey && !target.AllowBearer {
			problems = append(problems, prefix+" api_key must not be empty when allow_bearer_passthrough is false")
		}
		for j, m := range target.AllowedModels {
			if strings.TrimSpace(m) == "" {
				problems = append(problems, fmt.Sprintf("%s allowed_models[%d] must not be empty", prefix, j))
			}
		}
	}

	if strings.TrimSpace(c.DataStore.DBPath) == "" {
		problems = append(problems, "data_store.db_path must not be empty")
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
	if len(c.Targets) > 0 {
		clone.Targets = make([]Target, len(c.Targets))
		for i := range c.Targets {
			clone.Targets[i] = c.Targets[i]
			if len(c.Targets[i].AllowedModels) > 0 {
				clone.Targets[i].AllowedModels = append([]string(nil), c.Targets[i].AllowedModels...)
			}
			if len(c.Targets[i].APIKeys) > 0 {
				clone.Targets[i].APIKeys = append([]string(nil), c.Targets[i].APIKeys...)
			}
		}
	}

	return &clone
}

func resolveDataFilePaths(cfg *Config, baseDir string) {
	if cfg == nil {
		return
	}
	cfg.DataStore.DBPath = resolvePath(baseDir, cfg.DataStore.DBPath)
	cfg.DataFiles.ClientsFile = resolvePath(baseDir, cfg.DataFiles.ClientsFile)
	cfg.DataFiles.ModelCostsFile = resolvePath(baseDir, cfg.DataFiles.ModelCostsFile)
	cfg.DataFiles.UsageEventsFile = resolvePath(baseDir, cfg.DataFiles.UsageEventsFile)
	cfg.DataFiles.AdminUsersFile = resolvePath(baseDir, cfg.DataFiles.AdminUsersFile)
	cfg.DataFiles.AdminAuditFile = resolvePath(baseDir, cfg.DataFiles.AdminAuditFile)
	cfg.Logging.AccessLog = resolvePath(baseDir, cfg.Logging.AccessLog)
	cfg.Logging.ErrorLog = resolvePath(baseDir, cfg.Logging.ErrorLog)
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
