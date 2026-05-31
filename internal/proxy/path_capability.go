package proxy

import (
	"strings"

	"github.com/ycgame/llms-proxy/internal/config"
)

type requestSchema string

const (
	requestSchemaOpenAICompat    requestSchema = "openai-compatible"
	requestSchemaOpenAIChat      requestSchema = "openai-chat"
	requestSchemaOpenAIResponses requestSchema = "openai-responses"
	requestSchemaAnthropic       requestSchema = "anthropic"
)

// wangsuOpenAISupportedPaths 列出 wangsu_openai 支持的请求路径后缀。
// 网宿 OpenAI 兼容通道仅支持部分 OpenAI 端点，不支持 /responses 等。
var wangsuOpenAISupportedPaths = []string{
	"/chat/completions",
	"/images/generations",
	"/images/edits",
	"/images/variations",
	"/embeddings",
}

// 网宿图像通道（独立终态 URL），各自只接受一条客户端路径，用以路由消歧。
var (
	wangsuOpenAIImageSupportedPaths     = []string{"/images/generations"}
	wangsuOpenAIImageEditSupportedPaths = []string{"/images/edits"}
)

// PathSupportedByEndpointType 检查指定 endpoint_type 是否支持给定的请求路径 schema。
// 目标选择先按 model 过滤，再按请求 schema 过滤，避免同一 model 在 OpenAI / Anthropic /
// Responses 多种上游格式间负载时被路由到不兼容的 target。
func PathSupportedByEndpointType(epType, path string) bool {
	pathLower := strings.ToLower(path)
	schema := requestSchemaForPath(pathLower)
	switch epType {
	case config.EndpointTypeOpenAI, config.EndpointTypeAzureOpenAI:
		return schema != requestSchemaAnthropic
	case config.EndpointTypeClaude, config.EndpointTypeWangsuClaude:
		return schema == requestSchemaAnthropic
	case config.EndpointTypeGemini, config.EndpointTypeWangsuGemini, config.EndpointTypeCopilot:
		return schema == requestSchemaOpenAICompat || schema == requestSchemaOpenAIChat
	case config.EndpointTypeDeepSeek, config.EndpointTypeBailian:
		return schema == requestSchemaOpenAICompat || schema == requestSchemaOpenAIChat || schema == requestSchemaAnthropic
	case config.EndpointTypeBailianAPI:
		return schema == requestSchemaOpenAICompat || schema == requestSchemaOpenAIChat || schema == requestSchemaOpenAIResponses || schema == requestSchemaAnthropic
	case config.EndpointTypeWangsuOpenAI:
		if schema == requestSchemaAnthropic || schema == requestSchemaOpenAIResponses {
			return false
		}
		return pathHasAnySuffix(pathLower, wangsuOpenAISupportedPaths)
	case config.EndpointTypeWangsuOpenAIImage:
		return pathHasAnySuffix(pathLower, wangsuOpenAIImageSupportedPaths)
	case config.EndpointTypeWangsuOpenAIImageEdit:
		return pathHasAnySuffix(pathLower, wangsuOpenAIImageEditSupportedPaths)
	}
	return schema != requestSchemaAnthropic
}

func requestSchemaForPath(pathLower string) requestSchema {
	pathLower = strings.TrimSpace(pathLower)
	switch {
	case isAnthropicStylePath(pathLower):
		return requestSchemaAnthropic
	case isOpenAIResponsesStylePath(pathLower):
		return requestSchemaOpenAIResponses
	case pathHasAnySuffix(pathLower, []string{"/chat/completions"}):
		return requestSchemaOpenAIChat
	default:
		return requestSchemaOpenAICompat
	}
}

func pathHasAnySuffix(pathLower string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(pathLower, suf) {
			return true
		}
	}
	return false
}
