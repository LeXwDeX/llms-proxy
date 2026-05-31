package proxy

import (
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestPathSupportedByEndpointType(t *testing.T) {
	tests := []struct {
		epType string
		path   string
		want   bool
	}{
		// OpenAI 兼容类型支持 OpenAI Chat / Responses，不接 Anthropic Messages
		{config.EndpointTypeOpenAI, "/v1/responses", true},
		{config.EndpointTypeOpenAI, "/v1/chat/completions", true},
		{config.EndpointTypeOpenAI, "/v1/messages", false},
		{config.EndpointTypeAzureOpenAI, "/v1/responses", true},
		{config.EndpointTypeAzureOpenAI, "/v1/messages", false},
		// Anthropic 类型只接 Anthropic Messages
		{config.EndpointTypeClaude, "/v1/messages", true},
		{config.EndpointTypeClaude, "/v1/chat/completions", false},
		{config.EndpointTypeClaude, "/v1/responses", false},
		// Gemini / Copilot 视为 OpenAI Chat 兼容，不接 Anthropic / Responses
		{config.EndpointTypeGemini, "/v1/models/gemini:generateContent", true},
		{config.EndpointTypeGemini, "/v1/chat/completions", true},
		{config.EndpointTypeGemini, "/v1/messages", false},
		{config.EndpointTypeGemini, "/v1/responses", false},
		// 双协议类型按 path schema 自动分流
		{config.EndpointTypeDualProtocol, "/v1/messages", true},
		{config.EndpointTypeDualProtocol, "/v1/chat/completions", true},
		{config.EndpointTypeDualProtocol, "/v1/responses", true},
		// openai_image：仅图片路径
		{config.EndpointTypeOpenAIImage, "/v1/images/generations", true},
		{config.EndpointTypeOpenAIImage, "/v1/images/edits", true},
		{config.EndpointTypeOpenAIImage, "/v1/images/variations", true},
		{config.EndpointTypeOpenAIImage, "/v1/chat/completions", false},
		{config.EndpointTypeOpenAIImage, "/", false},
	}
	for _, tt := range tests {
		got := PathSupportedByEndpointType(tt.epType, tt.path)
		if got != tt.want {
			t.Errorf("PathSupportedByEndpointType(%q, %q) = %v, want %v", tt.epType, tt.path, got, tt.want)
		}
	}
}

func TestOpenAIImageOperationSupportsVariationsPath(t *testing.T) {
	if !openAIImageOperationSupportsPath(config.ImageOperationVariations, "/v1/images/variations") {
		t.Fatal("expected variations operation to support /v1/images/variations")
	}
	if openAIImageOperationSupportsPath(config.ImageOperationVariations, "/v1/images/generations") {
		t.Fatal("expected variations operation to reject /v1/images/generations")
	}
}
