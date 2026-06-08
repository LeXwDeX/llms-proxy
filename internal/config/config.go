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
	EndpointTypeOpenAIImage  = "openai_image"  // OpenAI 图片生成/编辑（独立终态 URL）
	EndpointTypeDualProtocol = "dual_protocol" // 双协议兼容（OpenAI + Anthropic，按路径自动识别，prefix 由 target 配置）
)

const (
	ImageOperationGenerations = "generations"
	ImageOperationEdits       = "edits"
	ImageOperationVariations  = "variations"
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
	switch t {
	case "deepseek", "bailian", "bailian_api":
		return EndpointTypeDualProtocol
	case "wangsu_openai":
		return EndpointTypeOpenAI
	case "wangsu_claude":
		return EndpointTypeClaude
	case "wangsu_gemini":
		return EndpointTypeGemini
	case "wangsu_openai_image", "wangsu_openai_image_edit":
		return EndpointTypeOpenAIImage
	}
	return t
}

// NormalizeTargetForCompatibility returns a target with legacy endpoint fields normalized.
func NormalizeTargetForCompatibility(target Target) Target {
	rawType := strings.ToLower(strings.TrimSpace(target.EndpointType))
	target.EndpointType = NormalizeEndpointType(target.EndpointType)
	target.ImageOperation = NormalizeImageOperation(target.ImageOperation)
	switch rawType {
	case "deepseek":
		if strings.TrimSpace(target.AnthropicPrefix) == "" {
			target.AnthropicPrefix = "/anthropic"
		}
	case "bailian", "bailian_api":
		if strings.TrimSpace(target.OpenAIPrefix) == "" {
			target.OpenAIPrefix = "/compatible-mode"
		}
		if strings.TrimSpace(target.AnthropicPrefix) == "" {
			target.AnthropicPrefix = "/apps/anthropic"
		}
		target.SupportsResponses = true
	case "wangsu_openai_image":
		target.ImageOperation = ImageOperationGenerations
	case "wangsu_openai_image_edit":
		target.ImageOperation = ImageOperationEdits
	}
	if target.EndpointType == EndpointTypeOpenAIImage && target.ImageOperation == "" {
		target.ImageOperation = InferImageOperationFromEndpoint(target.Endpoint)
	}
	return target
}

// NormalizeImageOperation returns a canonical image operation value.
func NormalizeImageOperation(op string) string {
	return strings.ToLower(strings.TrimSpace(op))
}

// InferImageOperationFromEndpoint infers the image operation for legacy openai_image targets.
func InferImageOperationFromEndpoint(endpoint string) string {
	endpoint = strings.ToLower(endpoint)
	if strings.Contains(endpoint, "variation") {
		return ImageOperationVariations
	}
	if strings.Contains(endpoint, "edit") {
		return ImageOperationEdits
	}
	return ImageOperationGenerations
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

// ModelMapping maps an upstream model name to an optional downstream fallback alias.
//
// When a client requests a model by its fallback alias, the proxy resolves it
// to the upstream name before forwarding. The upstream name is always preserved
// (original case) in the forwarded request.
type ModelMapping struct {
	Upstream string `json:"upstream"`             // 必填，上游名称
	Fallback string `json:"fallback,omitempty"`   // 可选，下游兜底别名
}

// Target represents one upstream endpoint (Azure OpenAI, OpenAI, Claude, Gemini, OpenAI Image, etc.).
type Target struct {
	Name               string   `json:"name"`
	EndpointType       string   `json:"endpoint_type,omitempty"` // azure_openai | openai | claude | gemini | openai_image | dual_protocol; default azure_openai
	Endpoint           string   `json:"endpoint"`
	ResourcePathPrefix string   `json:"resource_path_prefix"`
	APIKey             string   `json:"api_key"`
	APIKeys            []string `json:"api_keys,omitempty"`       // 额外 key 池（与 api_key 合并为有序池）
	KeyResetTime       string   `json:"key_reset_time,omitempty"` // 额度重置时间点（CST），格式 "23"/"monthly:23"（每月23号）或 "2006-01-02"/"2006-01-02 15:04"
	ProviderClass      string   `json:"provider_class,omitempty"` // subscription | pay_as_you_go; 影响限流/超额/额度耗尽的处理策略
	Paused             bool     `json:"paused,omitempty"`
	AllowBearer        bool     `json:"allow_bearer_passthrough"`
	AuthMode           string   `json:"auth_mode,omitempty"` // "bearer" | "" (default: x-api-key for claude types)
	ImageOperation     string   `json:"image_operation,omitempty"`
	ModelMappings      []ModelMapping `json:"model_mappings,omitempty"`
	SSEAutoAggregate   *bool    `json:"sse_auto_aggregate,omitempty"` // nil defaults to true
	// dual_protocol 专用字段
	OpenAIPrefix      string `json:"openai_prefix,omitempty"`      // OpenAI 路径前缀，如 "/compatible-mode"
	AnthropicPrefix   string `json:"anthropic_prefix,omitempty"`   // Anthropic 路径前缀，如 "/apps/anthropic"
	SupportsResponses bool   `json:"supports_responses,omitempty"` // 是否支持 /v1/responses
}

// Client describes a consumer and its access rights.
type Client struct {
	Name            string   `json:"name"`
	AccessKey       string   `json:"access_key"`
	AllowedTargets  []string `json:"allowed_targets"`
	QuotaDailyUSD   float64  `json:"quota_daily_usd,omitempty"`
	QuotaWeeklyUSD  float64  `json:"quota_weekly_usd,omitempty"`
	QuotaMonthlyUSD float64  `json:"quota_monthly_usd,omitempty"`
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

// RemoveTargetsFromFile removes legacy target definitions from a config file
// after they have been migrated into the datastore.
func RemoveTargetsFromFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(content, &raw); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	if _, ok := raw["targets"]; !ok {
		return nil
	}
	delete(raw, "targets")
	payload, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", path, err)
	}
	payload = append(payload, '\n')
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return fmt.Errorf("config: write temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: replace %s: %w", path, err)
	}
	return nil
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
		target = NormalizeTargetForCompatibility(target)
		c.Targets[i] = target
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
		upstreamSet := make(map[string]bool)
		fallbackSet := make(map[string]bool)
		for j, mm := range target.ModelMappings {
			up := strings.TrimSpace(mm.Upstream)
			if up == "" {
				problems = append(problems, fmt.Sprintf("%s model_mappings[%d].upstream must not be empty", prefix, j))
				continue
			}
			upLow := strings.ToLower(up)
			if upstreamSet[upLow] {
				problems = append(problems, fmt.Sprintf("%s model_mappings[%d].upstream %q is duplicate (case-insensitive)", prefix, j, up))
			}
			upstreamSet[upLow] = true
			if fb := strings.TrimSpace(mm.Fallback); fb != "" {
				fbLow := strings.ToLower(fb)
				// Fallback may not be empty (already trimmed)
				if upstreamSet[fbLow] {
					problems = append(problems, fmt.Sprintf("%s model_mappings[%d].fallback %q conflicts with an upstream name", prefix, j, fb))
				}
				if fallbackSet[fbLow] {
					problems = append(problems, fmt.Sprintf("%s model_mappings[%d].fallback %q is duplicate (case-insensitive)", prefix, j, fb))
				}
				fallbackSet[fbLow] = true
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
			if len(c.Targets[i].APIKeys) > 0 {
				clone.Targets[i].APIKeys = append([]string(nil), c.Targets[i].APIKeys...)
			}
			if len(c.Targets[i].ModelMappings) > 0 {
				clone.Targets[i].ModelMappings = append([]ModelMapping(nil), c.Targets[i].ModelMappings...)
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
