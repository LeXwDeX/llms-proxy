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
		// 官方类型全放行
		{config.EndpointTypeOpenAI, "/v1/responses", true},
		{config.EndpointTypeOpenAI, "/v1/chat/completions", true},
		{config.EndpointTypeAzureOpenAI, "/v1/responses", true},
		{config.EndpointTypeClaude, "/v1/messages", true},
		{config.EndpointTypeGemini, "/v1/models/gemini:generateContent", true},
		// wangsu_claude / wangsu_gemini 全放行
		{config.EndpointTypeWangsuClaude, "/v1/messages", true},
		{config.EndpointTypeWangsuClaude, "/v1/responses", true},
		{config.EndpointTypeWangsuGemini, "/v1/models/gemini:generateContent", true},
		// wangsu_openai 受限
		{config.EndpointTypeWangsuOpenAI, "/v1/chat/completions", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/images/generations", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/images/edits", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/images/variations", true},
		{config.EndpointTypeWangsuOpenAI, "/v1/embeddings", true},
		{config.EndpointTypeWangsuOpenAI, "/openai/deployments/gpt-4o/chat/completions", true},
		// wangsu_openai 不支持
		{config.EndpointTypeWangsuOpenAI, "/v1/responses", false},
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
