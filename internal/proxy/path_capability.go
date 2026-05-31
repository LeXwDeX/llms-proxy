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

// openAIImageSupportedPaths 列出 openai_image 支持的请求路径后缀。
var openAIImageSupportedPaths = []string{
	"/images/generations",
	"/images/edits",
	"/images/variations",
}

// PathSupportedByEndpointType 检查指定 endpoint_type 是否支持给定的请求路径 schema。
// 目标选择先按 model 过滤，再按请求 schema 过滤，避免同一 model 在 OpenAI / Anthropic /
// Responses 多种上游格式间负载时被路由到不兼容的 target。
func PathSupportedByEndpointType(epType, path string) bool {
	pathLower := strings.ToLower(path)
	schema := requestSchemaForPath(pathLower)
	switch epType {
	case config.EndpointTypeOpenAI, config.EndpointTypeAzureOpenAI:
		return schema != requestSchemaAnthropic
	case config.EndpointTypeClaude:
		return schema == requestSchemaAnthropic
	case config.EndpointTypeGemini:
		return schema == requestSchemaOpenAICompat || schema == requestSchemaOpenAIChat
	case config.EndpointTypeDualProtocol:
		return schema == requestSchemaOpenAICompat || schema == requestSchemaOpenAIChat || schema == requestSchemaOpenAIResponses || schema == requestSchemaAnthropic
	case config.EndpointTypeOpenAIImage:
		return pathHasAnySuffix(pathLower, openAIImageSupportedPaths)
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

func openAIImageOperationSupportsPath(operation, path string) bool {
	pathLower := strings.ToLower(path)
	switch operation {
	case config.ImageOperationEdits:
		return strings.HasSuffix(pathLower, "/images/edits")
	case config.ImageOperationVariations:
		return strings.HasSuffix(pathLower, "/images/variations")
	case config.ImageOperationGenerations, "":
		return strings.HasSuffix(pathLower, "/images/generations")
	default:
		return false
	}
}
