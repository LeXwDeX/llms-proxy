package proxy

import (
	"net/url"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestDeduplicateV1Path(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// 无重复，原样返回
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/compatible-mode/v1/chat/completions", "/compatible-mode/v1/chat/completions"},
		{"/apps/anthropic/v1/messages", "/apps/anthropic/v1/messages"},
		{"/v1/responses", "/v1/responses"},
		{"/", "/"},
		{"", ""},

		// /v1/v1 重复 → 去重
		{"/v1/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/v1/responses", "/v1/responses"},
		{"/v1/v1/embeddings", "/v1/embeddings"},
		{"/v1/v1/messages", "/v1/messages"},
		{"/v1/v1", "/v1"},

		// 带前缀的 /v1/v1 重复
		{"/compatible-mode/v1/v1/chat/completions", "/compatible-mode/v1/chat/completions"},
		{"/compatible-mode/v1/v1/responses", "/compatible-mode/v1/responses"},
		{"/apps/anthropic/v1/v1/messages", "/apps/anthropic/v1/messages"},

		// 不应误伤：/v2/v1 不是重复
		{"/v2/v1/chat/completions", "/v2/v1/chat/completions"},
		// /v1/v2 不是重复
		{"/v1/v2/something", "/v1/v2/something"},
	}
	for _, tc := range cases {
		got := deduplicateV1Path(tc.input)
		if got != tc.want {
			t.Errorf("deduplicateV1Path(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestBuildURLOpenAIDeduplicatesV1Path(t *testing.T) {
	// 场景：target endpoint URL 自带 /v1，客户端也发 /v1/...，不应出现 /v1/v1
	cases := []struct {
		name       string
		endpoint   string
		clientPath string
		want       string
	}{
		{
			name:       "endpoint with /v1 + client /v1/chat/completions",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/v1/chat/completions",
			want:       "https://api.openai.com/v1/chat/completions",
		},
		{
			name:       "endpoint with /v1 + client /v1/responses",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/v1/responses",
			want:       "https://api.openai.com/v1/responses",
		},
		{
			name:       "endpoint with /v1 + client /v1/embeddings",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/v1/embeddings",
			want:       "https://api.openai.com/v1/embeddings",
		},
		{
			name:       "endpoint without /v1 + client /v1/chat/completions",
			endpoint:   "https://api.openai.com",
			clientPath: "/v1/chat/completions",
			want:       "https://api.openai.com/v1/chat/completions",
		},
		{
			name:       "endpoint without /v1 + client /chat/completions",
			endpoint:   "https://api.openai.com/v1",
			clientPath: "/chat/completions",
			want:       "https://api.openai.com/v1/chat/completions",
		},
	}
	s := &Service{providerRegistry: DefaultProviderRegistry()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, _ := url.Parse(tc.endpoint)
			target := &Target{
				Name:         "openai-test",
				EndpointType: config.EndpointTypeOpenAI,
				Endpoint:     endpoint,
			}
			got, err := s.buildURL(target, &url.URL{Path: tc.clientPath})
			if err != nil {
				t.Fatalf("buildURL error: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("buildURL = %q, want %q", got.String(), tc.want)
			}
		})
	}
}
