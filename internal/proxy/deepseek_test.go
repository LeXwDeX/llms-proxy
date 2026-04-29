// deepseek_test.go — DeepSeek 路径自动识别 + endpoint_type 路由约束的回归测试。
package proxy

import (
	"net/url"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestBuildURLDeepSeekOpenAIPathPassthrough(t *testing.T) {
	endpoint, _ := url.Parse("https://api.deepseek.com")
	target := &Target{
		Name:         "ds",
		EndpointType: config.EndpointTypeDeepSeek,
		Endpoint:     endpoint,
	}
	s := &Service{}

	cases := []struct {
		clientPath string
		want       string
	}{
		{"/v1/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"/chat/completions", "https://api.deepseek.com/chat/completions"},
		{"/v1/embeddings", "https://api.deepseek.com/v1/embeddings"},
		{"/v1/models", "https://api.deepseek.com/v1/models"},
	}
	for _, tc := range cases {
		original := &url.URL{Path: tc.clientPath}
		got, err := s.buildURL(target, original)
		if err != nil {
			t.Fatalf("buildURL(%q) error: %v", tc.clientPath, err)
		}
		if got.String() != tc.want {
			t.Errorf("buildURL(%q) = %q, want %q", tc.clientPath, got.String(), tc.want)
		}
	}
}

func TestBuildURLDeepSeekAnthropicPathPrefix(t *testing.T) {
	endpoint, _ := url.Parse("https://api.deepseek.com")
	target := &Target{
		Name:         "ds",
		EndpointType: config.EndpointTypeDeepSeek,
		Endpoint:     endpoint,
	}
	s := &Service{}

	cases := []struct {
		clientPath string
		want       string
	}{
		{"/v1/messages", "https://api.deepseek.com/anthropic/v1/messages"},
		{"/v1/messages/count_tokens", "https://api.deepseek.com/anthropic/v1/messages/count_tokens"},
	}
	for _, tc := range cases {
		original := &url.URL{Path: tc.clientPath}
		got, err := s.buildURL(target, original)
		if err != nil {
			t.Fatalf("buildURL(%q) error: %v", tc.clientPath, err)
		}
		if got.String() != tc.want {
			t.Errorf("buildURL(%q) = %q, want %q", tc.clientPath, got.String(), tc.want)
		}
	}
}

func TestBuildURLDeepSeekDoesNotRewriteForOtherEndpointTypes(t *testing.T) {
	// Anthropic 风格 path 不应影响非 DeepSeek 类型的 target（例如真正的 Claude）。
	endpoint, _ := url.Parse("https://api.anthropic.com")
	target := &Target{
		Name:         "claude-real",
		EndpointType: config.EndpointTypeClaude,
		Endpoint:     endpoint,
	}
	s := &Service{}
	original := &url.URL{Path: "/v1/messages"}
	got, err := s.buildURL(target, original)
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}
	want := "https://api.anthropic.com/v1/messages"
	if got.String() != want {
		t.Errorf("buildURL = %q, want %q (no /anthropic prefix for claude type)", got.String(), want)
	}
}

func TestIsAnthropicStylePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/messages", true},
		{"/v1/messages/count_tokens", true},
		{"/V1/Messages", true},
		{"  /v1/messages  ", true},
		{"/v1/chat/completions", false},
		{"/chat/completions", false},
		{"/v1/embeddings", false},
		{"/v1/models", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isAnthropicStylePath(tc.path); got != tc.want {
			t.Errorf("isAnthropicStylePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
