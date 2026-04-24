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

	// 网宿图像通道：endpoint 是终态 URL（如 .../openai-image），不拼接客户端 path。
	// 客户端发 POST /v1/images/generations，但上游只认固定 URL，因此整体覆盖。
	if isTerminalEndpointType(target.EndpointType) {
		forward.RawQuery = normalizeForwardQuery(original)
		return &forward, nil
	}

	path := mergePaths(target.ResourcePathPrefix, original.Path)

	// Concatenate paths explicitly instead of using url.URL.Parse, because
	// url.Parse treats paths starting with "/" as absolute and would discard
	// any sub-path already present in the endpoint (e.g. a gateway base path
	// like /v2/gws/<id>/anthropic).
	forward.Path = strings.TrimRight(forward.Path, "/") + "/" + strings.TrimLeft(path, "/")
	forward.RawQuery = normalizeForwardQuery(original)
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

func deleteQueryKeyCaseInsensitive(query url.Values, key string) {
	for existing := range query {
		if strings.EqualFold(existing, key) {
			delete(query, existing)
		}
	}
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
