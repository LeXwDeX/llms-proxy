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
		{config.EndpointTypeDeepSeek, "/v1/messages", true},
		{config.EndpointTypeDeepSeek, "/v1/chat/completions", true},
		{config.EndpointTypeDeepSeek, "/v1/responses", false},
		{config.EndpointTypeBailian, "/v1/messages", true},
		{config.EndpointTypeBailian, "/v1/chat/completions", true},
		{config.EndpointTypeBailian, "/v1/responses", false},
		{config.EndpointTypeBailianAPI, "/v1/messages", true},
		{config.EndpointTypeBailianAPI, "/v1/chat/completions", true},
		{config.EndpointTypeBailianAPI, "/v1/responses", true},
		// wangsu_claude 仅 Anthropic，wangsu_gemini 仅 OpenAI Chat 兼容
		{config.EndpointTypeWangsuClaude, "/v1/messages", true},
		{config.EndpointTypeWangsuClaude, "/v1/responses", false},
		{config.EndpointTypeWangsuGemini, "/v1/models/gemini:generateContent", true},
		{config.EndpointTypeWangsuGemini, "/v1/messages", false},
		{config.EndpointTypeWangsuGemini, "/v1/responses", false},
		// wangsu_openai 受限
		{config.EndpointTypeWangsuOpenAI, "/v1/chat/completions", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/images/generations", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/images/edits", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/images/variations", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/embeddings", true},
		{config.EndpointTypeWangsuOpenAI, "/openai/deployments/gpt-4o/chat/completions", true},
		// wangsu_openai 不支持
		{config.EndpointTypeWangsuOpenAI, "/v1/responses", false},
		{config.EndpointTypeWangsuOpenAI, "/v1/messages", false},
		{config.EndpointTypeWangsuOpenAI, "/v1/audio/transcriptions", false},
		{config.EndpointTypeWangsuOpenAI, "/v1/models", false},
		{config.EndpointTypeWangsuOpenAI, "/", false},
		// wangsu_openai_image：仅文生图
		{config.EndpointTypeWangsuOpenAIImage, "/v1/images/generations", true},
		{config.EndpointTypeWangsuOpenAIImage, "/v1/images/edits", false},
		{config.EndpointTypeWangsuOpenAIImage, "/v1/chat/completions", false},
		{config.EndpointTypeWangsuOpenAIImage, "/", false},
		// wangsu_openai_image_edit：仅图编辑
		{config.EndpointTypeWangsuOpenAIImageEdit, "/v1/images/edits", true},
		{config.EndpointTypeWangsuOpenAIImageEdit, "/v1/images/generations", false},
		{config.EndpointTypeWangsuOpenAIImageEdit, "/v1/chat/completions", false},
	}
	for _, tt := range tests {
		got := PathSupportedByEndpointType(tt.epType, tt.path)
		if got != tt.want {
			t.Errorf("PathSupportedByEndpointType(%q, %q) = %v, want %v", tt.epType, tt.path, got, tt.want)
		}
	}
}
