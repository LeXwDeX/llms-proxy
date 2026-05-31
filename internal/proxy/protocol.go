// protocol.go — 协议解析与协议-路径兼容性检查。
// 新增文件，不替换 path_capability.go 中的旧逻辑。
package proxy

import (
	"strings"

	"github.com/ycgame/llms-proxy/internal/config"
)

// ResolveProtocol 从客户端请求路径解析协议类型。
// 判断顺序：Anthropic path → Responses path → Image path → Gemini path → Chat path → 默认 OpenAIChat
func ResolveProtocol(path string) config.ProtocolType {
	pl := strings.ToLower(strings.TrimSpace(path))
	switch {
	case isAnthropicStylePath(pl):
		return config.ProtocolAnthropicMessages
	case isOpenAIResponsesStylePath(pl):
		return config.ProtocolOpenAIResponses
	case isOpenAIImageStylePath(pl):
		return config.ProtocolOpenAIImage
	case isGeminiStylePath(pl):
		return config.ProtocolGemini
	case pathHasAnySuffix(pl, []string{"/chat/completions"}):
		return config.ProtocolOpenAIChat
	default:
		return config.ProtocolOpenAIChat // 默认 OpenAI 兼容
	}
}

// ProtocolSupportsPath 检查协议类型是否支持给定路径。
func ProtocolSupportsPath(protocol config.ProtocolType, path string) bool {
	resolved := ResolveProtocol(path)
	switch protocol {
	case config.ProtocolOpenAIChat:
		return resolved == config.ProtocolOpenAIChat
	case config.ProtocolOpenAIResponses:
		return resolved == config.ProtocolOpenAIResponses
	case config.ProtocolAnthropicMessages:
		return resolved == config.ProtocolAnthropicMessages
	case config.ProtocolGemini:
		return resolved == config.ProtocolGemini
	case config.ProtocolOpenAIImage:
		return resolved == config.ProtocolOpenAIImage
	}
	return false
}

// isOpenAIImageStylePath 判断路径是否为 OpenAI 图像 API 风格。
func isOpenAIImageStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return strings.HasSuffix(pl, "/images/generations") ||
		strings.HasSuffix(pl, "/images/edits") ||
		strings.HasSuffix(pl, "/images/variations")
}

// isGeminiStylePath 判断路径是否为 Gemini API 风格。
// Gemini 路径形如 /v1beta/models/{model}:generateContent。
func isGeminiStylePath(p string) bool {
	pl := strings.ToLower(strings.TrimSpace(p))
	return (strings.Contains(pl, "/models/") && strings.Contains(pl, ":generatecontent")) ||
		(strings.Contains(pl, "/models/") && strings.Contains(pl, ":streamgeneratecontent"))
}
