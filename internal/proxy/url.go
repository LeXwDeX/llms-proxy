// url.go — 上游 URL 拼装与查询参数清洗。
package proxy

import (
	"errors"
	"net/url"
	"strings"

	"github.com/ycgame/llms-proxy/internal/config"
)

func (s *Service) buildURL(target *Target, original *url.URL) (*url.URL, error) {
	if target == nil || target.Endpoint == nil {
		return nil, errors.New("target not configured")
	}

	forward := *target.Endpoint
	forward.RawQuery = ""
	forward.Fragment = ""

	// 查找 provider profile
	var profile *ProviderProfile
	if s.providerRegistry != nil {
		profile = s.providerRegistry.Lookup(target.EndpointType)
	}

	// 终态 URL：不拼接客户端 path（如网宿图像通道）。
	// 客户端发 POST /v1/images/generations，但上游只认固定 URL，因此整体覆盖。
	if profile != nil && profile.Path.IsTerminalURL() {
		forward.RawQuery = normalizeForwardQuery(original)
		return &forward, nil
	}
	if profile == nil && isTerminalEndpointType(target.EndpointType) {
		forward.RawQuery = normalizeForwardQuery(original)
		return &forward, nil
	}

	// 使用 PathMapper 改写路径
	var path string
	if profile != nil {
		// 准备 endpoint path（BailianAPI 会 strip 已知 base path）
		forward.Path = profile.Path.PrepareEndpointPath(forward.Path)
		// 改写客户端路径
		path = profile.Path.RewritePath(original.Path, target.ResourcePathPrefix)
	} else {
		// 兼容旧逻辑（不应到达，因为所有 endpoint_type 都已注册）
		path = mergePaths(target.ResourcePathPrefix, original.Path)
	}

	// Concatenate paths explicitly instead of using url.URL.Parse, because
	// url.Parse treats paths starting with "/" as absolute and would discard
	// any sub-path already present in the endpoint (e.g. a gateway base path
	// like /v2/gws/<id>/anthropic).
	forward.Path = strings.TrimRight(forward.Path, "/") + "/" + strings.TrimLeft(path, "/")
	forward.Path = deduplicateV1Path(forward.Path)
	forward.RawQuery = normalizeForwardQuery(original)

	// Azure OpenAI deployment-based API（/deployments/{name}/...）需要 api-version 查询参数。
	// v1 API（/openai/v1/...）明确不需要 api-version，preview 特性通过 feature-specific header 控制。
	// 参考：https://learn.microsoft.com/en-us/azure/foundry/openai/api-version-lifecycle
	// "The v1 API simplifies authentication, removes the need for dated api-version parameters"
	if target.EndpointType == config.EndpointTypeAzureOpenAI && isDeploymentBasedPath(forward.Path) {
		forward.RawQuery = appendAzureAPIVersion(forward.RawQuery, "2025-04-01-preview")
	}

	return &forward, nil
}

// isTerminalEndpointType 判断 endpoint_type 是否使用「终态 URL」语义
// （上游只接受固定 URL，buildURL 应整体覆盖客户端 path）。
func isTerminalEndpointType(epType string) bool {
	switch epType {
	case config.EndpointTypeWangsuOpenAIImage, config.EndpointTypeWangsuOpenAIImageEdit:
		return true
	}
	return false
}

// isDeploymentBasedPath 判断路径是否为 Azure 的 deployment-based 格式
// （如 /openai/deployments/gpt-4o/chat/completions），该格式需要 api-version 查询参数。
// v1 路径（如 /openai/v1/images/edits）不需要 api-version。
func isDeploymentBasedPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "/deployments/")
}

func normalizeForwardQuery(original *url.URL) string {
	if original == nil {
		return ""
	}

	query := original.Query()
	deleteQueryKeyCaseInsensitive(query, "target")
	deleteQueryKeyCaseInsensitive(query, "api-version")
	deleteQueryKeyCaseInsensitive(query, "api_version")
	deleteQueryKeyCaseInsensitive(query, "api-key")

	return query.Encode()
}

// appendAzureAPIVersion 向已编码的 query string 追加 api-version 参数。
// 如果已存在 api-version（不应发生，因为 normalizeForwardQuery 会剥离），则不重复追加。
func appendAzureAPIVersion(rawQuery, version string) string {
	if rawQuery == "" {
		return "api-version=" + url.QueryEscape(version)
	}
	return rawQuery + "&api-version=" + url.QueryEscape(version)
}

func deleteQueryKeyCaseInsensitive(query url.Values, key string) {
	for existing := range query {
		if strings.EqualFold(existing, key) {
			delete(query, existing)
		}
	}
}

// isAnthropicStylePath 判断给定客户端 path 是否走 Anthropic API 形态。
// DeepSeek Anthropic 兼容端口的路径以 /v1/messages 开头（包括 /v1/messages、
// /v1/messages/count_tokens 等子路径）。其余路径视为 OpenAI 兼容形态。
func isAnthropicStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return pl == "/v1/messages" || strings.HasPrefix(pl, "/v1/messages/") || strings.HasPrefix(pl, "/v1/messages?")
}

func isOpenAIResponsesStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return pl == "/v1/responses" || strings.HasPrefix(pl, "/v1/responses/") || strings.HasPrefix(pl, "/v1/responses?")
}

func stripBailianAPIBasePath(p string) string {
	clean := "/" + strings.Trim(strings.TrimSpace(p), "/")
	switch clean {
	case "/compatible-mode",
		"/compatible-mode/v1",
		"/apps/anthropic",
		"/api/v2/apps/protocols/compatible-mode",
		"/api/v2/apps/protocols/compatible-mode/v1":
		return ""
	default:
		return p
	}
}

func ensureLeadingSlash(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		return "/" + p
	}
	return p
}

// deduplicateV1Path 消除路径中因 endpoint URL 与客户端 path 都含 /v1 而产生的
// /v1/v1 重复前缀。例如 endpoint=https://api.openai.com/v1 + client=/v1/chat/completions
// 会拼出 /v1/v1/chat/completions，此函数将其归一化为 /v1/chat/completions。
// /v1/v1 在任何已知上游 API 中都不合法，因此该归一化是安全的。
func deduplicateV1Path(p string) string {
	// 处理路径中间的 /v1/v1/ 重复：保留第一个 /v1/，跳过第二个 /v1
	if idx := strings.Index(p, "/v1/v1/"); idx >= 0 {
		return p[:idx+1] + p[idx+4:]
	}
	// 处理路径以 /v1/v1 结尾（无后续路径）
	if strings.HasSuffix(p, "/v1/v1") {
		return p[:len(p)-3]
	}
	return p
}

func mergePaths(prefix, path string) string {
	if prefix == "" {
		if path == "" {
			return "/"
		}
		return path
	}
	if path == "" || path == "/" {
		return prefix
	}
	if strings.HasPrefix(path, prefix+"/") || path == prefix {
		return path
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(path, "/")
}
