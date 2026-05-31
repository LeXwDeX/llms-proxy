// url_provider_test.go — buildURL 通过 ProviderProfile.Path 改写路径的集成测试。
package proxy

import (
	"net/url"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestBuildURLWithProviderProfile(t *testing.T) {
	cases := []struct {
		name         string
		endpointType string
		endpoint     string
		clientPath   string
		wantURL      string
	}{
		// openai
		{"openai", config.EndpointTypeOpenAI, "https://api.openai.com", "/v1/chat/completions",
			"https://api.openai.com/v1/chat/completions"},

		// deepseek openai
		{"deepseek openai", config.EndpointTypeDeepSeek, "https://api.deepseek.com", "/v1/chat/completions",
			"https://api.deepseek.com/v1/chat/completions"},

		// deepseek anthropic
		{"deepseek anthropic", config.EndpointTypeDeepSeek, "https://api.deepseek.com", "/v1/messages",
			"https://api.deepseek.com/anthropic/v1/messages"},

		// bailian openai
		{"bailian openai", config.EndpointTypeBailian, "https://token-plan.cn-beijing.maas.aliyuncs.com", "/v1/chat/completions",
			"https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions"},

		// bailian anthropic
		{"bailian anthropic", config.EndpointTypeBailian, "https://token-plan.cn-beijing.maas.aliyuncs.com", "/v1/messages",
			"https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic/v1/messages"},

		// bailian_api openai (strip base path)
		{"bailian_api openai", config.EndpointTypeBailianAPI, "https://dashscope.aliyuncs.com/compatible-mode/v1", "/v1/chat/completions",
			"https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"},

		// bailian_api anthropic (strip base path)
		{"bailian_api anthropic", config.EndpointTypeBailianAPI, "https://dashscope.aliyuncs.com/compatible-mode/v1", "/v1/messages",
			"https://dashscope.aliyuncs.com/apps/anthropic/v1/messages"},

		// wangsu_openai_image (terminal URL)
		{"wangsu_image terminal", config.EndpointTypeWangsuOpenAIImage, "https://aigateway.example.com/openai-image", "/v1/images/generations",
			"https://aigateway.example.com/openai-image"},

		// azure deployment path
		{"azure deployment", config.EndpointTypeAzureOpenAI, "https://example.openai.azure.com", "/openai/deployments/gpt-4o/chat/completions",
			"https://example.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2025-04-01-preview"},
	}

	s := &Service{providerRegistry: DefaultProviderRegistry()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := url.Parse(tc.endpoint)
			target := &Target{
				Name:         tc.name,
				EndpointType: tc.endpointType,
				Endpoint:     endpoint,
			}
			got, err := s.buildURL(target, &url.URL{Path: tc.clientPath})
			if err != nil {
				t.Fatalf("buildURL error: %v", err)
			}
			if got.String() != tc.wantURL {
				t.Errorf("buildURL = %q, want %q", got.String(), tc.wantURL)
			}
		})
	}
}
