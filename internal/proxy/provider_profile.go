// provider_profile.go — ProviderProfile 接口、AuthStrategy / PathMapper 策略与 ProviderRegistry。
// 新增文件，不替换 forward.go / url.go 中的旧逻辑。
package proxy

import (
	"net/http"
	"strings"

	"github.com/ycgame/llms-proxy/internal/config"
)

// ---------------------------------------------------------------------------
// AuthStrategy — 上游认证头注入策略
// ---------------------------------------------------------------------------

// AuthStrategy 定义上游认证头注入策略。
type AuthStrategy interface {
	// InjectAuth 向上游请求注入认证头。
	// clientPath 是客户端原始路径（用于条件性注入 anthropic-version）。
	InjectAuth(req *http.Request, apiKey string, clientPath string) error
}

// BearerAuth 设置 Authorization: Bearer <key>
type BearerAuth struct{}

func (a *BearerAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return nil
}

// AnthropicAuth 设置 x-api-key + anthropic-version（或 Bearer 模式）
type AnthropicAuth struct {
	BearerMode bool // 当 AuthMode=="bearer" 时使用 Bearer 而非 x-api-key
}

func (a *AnthropicAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	if a.BearerMode {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("x-api-key", apiKey)
	}
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	return nil
}

// GeminiAuth 设置 x-goog-api-key
type GeminiAuth struct{}

func (a *GeminiAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	req.Header.Set("x-goog-api-key", apiKey)
	return nil
}

// BearerWithConditionalAnthropicVersion 设置 Bearer + 条件性 anthropic-version。
// 用于 bailian/bailian_api/deepseek：Bearer 认证，但 Anthropic 路径需要 anthropic-version。
type BearerWithConditionalAnthropicVersion struct{}

func (a *BearerWithConditionalAnthropicVersion) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if isAnthropicStylePath(clientPath) {
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	return nil
}

// AzureAuth 设置 api-key 或 Bearer passthrough
type AzureAuth struct {
	AllowBearer    bool
	AzureAuthValue string // 来自客户端 X-Azure-Authorization 头
}

func (a *AzureAuth) InjectAuth(req *http.Request, apiKey string, clientPath string) error {
	if a.AllowBearer && a.AzureAuthValue != "" {
		req.Header.Set("Authorization", a.AzureAuthValue)
	} else {
		req.Header.Set("api-key", apiKey)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PathMapper — 客户端路径到上游路径的改写策略
// ---------------------------------------------------------------------------

// PathMapper 定义客户端路径到上游路径的改写策略。
type PathMapper interface {
	// RewritePath 返回改写后的上游路径。
	RewritePath(clientPath string, resourcePathPrefix string) string
	// IsTerminalURL 返回 true 表示 endpoint URL 是终态 URL（不拼接客户端 path）。
	IsTerminalURL() bool
	// PrepareEndpointPath 返回经过 provider 特定处理后的 endpoint path。
	// 默认返回原值；BailianAPIPath 会调用 stripBailianAPIBasePath。
	PrepareEndpointPath(endpointPath string) string
}

// PassthroughPath 不做路径改写（仅 mergePaths）。
type PassthroughPath struct{}

func (m *PassthroughPath) RewritePath(clientPath, resourcePathPrefix string) string {
	return mergePaths(resourcePathPrefix, clientPath)
}

func (m *PassthroughPath) IsTerminalURL() bool { return false }

func (m *PassthroughPath) PrepareEndpointPath(endpointPath string) string { return endpointPath }

// DeepSeekPath DeepSeek 路径改写：Anthropic 路径加 /anthropic 前缀。
type DeepSeekPath struct{}

func (m *DeepSeekPath) RewritePath(clientPath, resourcePathPrefix string) string {
	path := mergePaths(resourcePathPrefix, clientPath)
	if isAnthropicStylePath(clientPath) {
		path = "/anthropic" + ensureLeadingSlash(path)
	}
	return path
}

func (m *DeepSeekPath) IsTerminalURL() bool { return false }

func (m *DeepSeekPath) PrepareEndpointPath(endpointPath string) string { return endpointPath }

// BailianPath 百炼 Token Plan 路径改写。
type BailianPath struct{}

func (m *BailianPath) RewritePath(clientPath, resourcePathPrefix string) string {
	path := mergePaths(resourcePathPrefix, clientPath)
	if isAnthropicStylePath(clientPath) {
		path = "/apps/anthropic" + ensureLeadingSlash(path)
	} else {
		path = "/compatible-mode" + ensureLeadingSlash(path)
	}
	return path
}

func (m *BailianPath) IsTerminalURL() bool { return false }

func (m *BailianPath) PrepareEndpointPath(endpointPath string) string { return endpointPath }

// BailianAPIPath 百炼 API 路径改写。
type BailianAPIPath struct {
	BasePath string // endpoint URL 的 path 部分，用于 stripBailianAPIBasePath
}

func (m *BailianAPIPath) RewritePath(clientPath, resourcePathPrefix string) string {
	path := mergePaths(resourcePathPrefix, clientPath)
	if isAnthropicStylePath(clientPath) {
		path = "/apps/anthropic" + ensureLeadingSlash(path)
	} else {
		path = "/compatible-mode" + ensureLeadingSlash(path)
	}
	return path
}

func (m *BailianAPIPath) IsTerminalURL() bool { return false }

func (m *BailianAPIPath) PrepareEndpointPath(endpointPath string) string {
	return stripBailianAPIBasePath(endpointPath)
}

// TerminalURLPath 终态 URL（不拼接客户端 path）。
type TerminalURLPath struct{}

func (m *TerminalURLPath) RewritePath(clientPath, resourcePathPrefix string) string {
	return "" // 不使用
}

func (m *TerminalURLPath) IsTerminalURL() bool { return true }

func (m *TerminalURLPath) PrepareEndpointPath(endpointPath string) string { return endpointPath }

// ---------------------------------------------------------------------------
// BodyPolicy — 请求体预处理策略
// ---------------------------------------------------------------------------

// BodyPolicy 定义请求体预处理策略。
type BodyPolicy struct {
	// PreserveMultipart 为 true 时保留 multipart/form-data 不转换。
	// Azure 和网宿图像端点原生支持 multipart。
	PreserveMultipart bool

	// SanitizeFunc 可选的请求体净化函数。
	// 仅 Azure 使用：白名单过滤不兼容字段。
	// 返回净化后的 body 和被剥离的字段名列表。
	SanitizeFunc func(r *http.Request, body []byte) ([]byte, []string)

	// InjectCacheControl 可选的 cache_control 注入函数。
	// 仅 Bailian Anthropic 路径使用。
	// path 参数为客户端原始路径，用于条件性注入。
	InjectCacheControl func(body []byte, path string) []byte
}

// ---------------------------------------------------------------------------
// ProviderProfile — provider 的协议能力与策略描述
// ---------------------------------------------------------------------------

// ProviderProfile 描述一个 provider 的协议能力和策略。
type ProviderProfile struct {
	EndpointType   string
	Provider       string
	Protocols      []config.ProtocolType
	Auth           AuthStrategy
	Path           PathMapper
	SupportedPaths []string // nil = 不限制（由协议决定）；非 nil = 仅支持这些路径后缀
	Body           BodyPolicy
}

// SupportsProtocol 检查 provider 是否支持给定协议。
func (p *ProviderProfile) SupportsProtocol(protocol config.ProtocolType) bool {
	for _, proto := range p.Protocols {
		if proto == protocol {
			return true
		}
	}
	return false
}

// SupportsPath 检查 provider 是否支持给定客户端路径。
// 先检查协议兼容性，再检查 SupportedPaths 限制。
func (p *ProviderProfile) SupportsPath(path string) bool {
	protocol := ResolveProtocol(path)
	if !p.SupportsProtocol(protocol) {
		return false
	}
	if p.SupportedPaths == nil {
		return true
	}
	pathLower := strings.ToLower(path)
	for _, sp := range p.SupportedPaths {
		if strings.HasSuffix(pathLower, sp) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// ProviderRegistry — endpoint_type → ProviderProfile 映射
// ---------------------------------------------------------------------------

// ProviderRegistry 将 endpoint_type 映射到 ProviderProfile。
type ProviderRegistry struct {
	profiles map[string]*ProviderProfile
}

// NewProviderRegistry 创建空的 ProviderRegistry。
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{profiles: make(map[string]*ProviderProfile)}
}

// Lookup 返回给定 endpoint_type 的 ProviderProfile，未找到返回 nil。
func (r *ProviderRegistry) Lookup(endpointType string) *ProviderProfile {
	return r.profiles[endpointType]
}

// Register 注册一个 ProviderProfile。
func (r *ProviderRegistry) Register(profile *ProviderProfile) {
	r.profiles[profile.EndpointType] = profile
}

// DefaultProviderRegistry 返回包含所有 13 个 endpoint_type 的默认注册表。
func DefaultProviderRegistry() *ProviderRegistry {
	r := NewProviderRegistry()

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeOpenAI,
		Provider:     "openai",
		Protocols:    []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolOpenAIImage},
		Auth:         &BearerAuth{},
		Path:         &PassthroughPath{},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeAzureOpenAI,
		Provider:     "azure",
		Protocols:    []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolOpenAIImage},
		Auth:         &AzureAuth{},
		Path:         &PassthroughPath{},
		Body: BodyPolicy{
			PreserveMultipart: true,
			SanitizeFunc:      sanitizeRequestBodyForAzure,
		},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeClaude,
		Provider:     "anthropic",
		Protocols:    []config.ProtocolType{config.ProtocolAnthropicMessages},
		Auth:         &AnthropicAuth{},
		Path:         &PassthroughPath{},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeGemini,
		Provider:     "google",
		Protocols:    []config.ProtocolType{config.ProtocolGemini},
		Auth:         &GeminiAuth{},
		Path:         &PassthroughPath{},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeDeepSeek,
		Provider:     "deepseek",
		Protocols:    []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolAnthropicMessages},
		Auth:         &BearerWithConditionalAnthropicVersion{},
		Path:         &DeepSeekPath{},
	})

	// bailianCacheControlInjector 是百炼 Anthropic 路径 cache_control 注入函数。
	// 仅当路径为 Anthropic 格式时注入，OpenAI 兼容路径不注入。
	bailianCacheControlInjector := func(body []byte, path string) []byte {
		if isAnthropicStylePath(path) {
			return injectBailianCacheControl(body)
		}
		return body
	}

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeBailian,
		Provider:     "bailian_token_plan",
		Protocols:    []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages},
		Auth:         &BearerWithConditionalAnthropicVersion{},
		Path:         &BailianPath{},
		Body: BodyPolicy{
			InjectCacheControl: bailianCacheControlInjector,
		},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeBailianAPI,
		Provider:     "bailian_api",
		Protocols:    []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages},
		Auth:         &BearerWithConditionalAnthropicVersion{},
		Path:         &BailianAPIPath{},
		Body: BodyPolicy{
			InjectCacheControl: bailianCacheControlInjector,
		},
	})

	r.Register(&ProviderProfile{
		EndpointType:   config.EndpointTypeWangsuOpenAI,
		Provider:       "wangsu",
		Protocols:      []config.ProtocolType{config.ProtocolOpenAIChat, config.ProtocolOpenAIImage},
		Auth:           &BearerAuth{},
		Path:           &PassthroughPath{},
		SupportedPaths: []string{"/chat/completions", "/images/generations", "/images/edits", "/images/variations", "/embeddings"},
	})

	r.Register(&ProviderProfile{
		EndpointType:   config.EndpointTypeWangsuOpenAIImage,
		Provider:       "wangsu",
		Protocols:      []config.ProtocolType{config.ProtocolOpenAIImage},
		Auth:           &BearerAuth{},
		Path:           &TerminalURLPath{},
		SupportedPaths: []string{"/images/generations"},
		Body: BodyPolicy{
			PreserveMultipart: true,
		},
	})

	r.Register(&ProviderProfile{
		EndpointType:   config.EndpointTypeWangsuOpenAIImageEdit,
		Provider:       "wangsu",
		Protocols:      []config.ProtocolType{config.ProtocolOpenAIImage},
		Auth:           &BearerAuth{},
		Path:           &TerminalURLPath{},
		SupportedPaths: []string{"/images/edits"},
		Body: BodyPolicy{
			PreserveMultipart: true,
		},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeWangsuClaude,
		Provider:     "wangsu",
		Protocols:    []config.ProtocolType{config.ProtocolAnthropicMessages},
		Auth:         &AnthropicAuth{},
		Path:         &PassthroughPath{},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeWangsuGemini,
		Provider:     "wangsu",
		Protocols:    []config.ProtocolType{config.ProtocolGemini},
		Auth:         &GeminiAuth{},
		Path:         &PassthroughPath{},
	})

	r.Register(&ProviderProfile{
		EndpointType: config.EndpointTypeCopilot,
		Provider:     "copilot",
		Protocols:    []config.ProtocolType{config.ProtocolOpenAIChat},
		Auth:         &BearerAuth{},
		Path:         &PassthroughPath{},
	})

	return r
}
