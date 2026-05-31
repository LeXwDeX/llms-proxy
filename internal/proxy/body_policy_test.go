package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

// ---------------------------------------------------------------------------
// TestBodyPolicyPreserveMultipart — 每个 provider 的 PreserveMultipart 配置
// ---------------------------------------------------------------------------

func TestBodyPolicyPreserveMultipart(t *testing.T) {
	registry := DefaultProviderRegistry()

	cases := []struct {
		endpointType string
		wantPreserve bool
	}{
		{config.EndpointTypeAzureOpenAI, true},
		{config.EndpointTypeOpenAIImage, true},
		{config.EndpointTypeOpenAI, false},
		{config.EndpointTypeClaude, false},
		{config.EndpointTypeGemini, false},
		{config.EndpointTypeDualProtocol, false},
	}

	for _, tc := range cases {
		t.Run(tc.endpointType, func(t *testing.T) {
			profile := registry.Lookup(tc.endpointType)
			if profile == nil {
				t.Fatalf("provider %q not registered", tc.endpointType)
			}
			if profile.Body.PreserveMultipart != tc.wantPreserve {
				t.Errorf("%s.Body.PreserveMultipart = %v, want %v",
					tc.endpointType, profile.Body.PreserveMultipart, tc.wantPreserve)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestBodyPolicySanitizeFunc — Azure 有 SanitizeFunc，其他 provider 无
// ---------------------------------------------------------------------------

func TestBodyPolicySanitizeFunc(t *testing.T) {
	registry := DefaultProviderRegistry()

	// Azure 有 SanitizeFunc
	azure := registry.Lookup(config.EndpointTypeAzureOpenAI)
	if azure == nil {
		t.Fatal("azure_openai not registered")
	}
	if azure.Body.SanitizeFunc == nil {
		t.Fatal("azure_openai should have SanitizeFunc")
	}

	// 其他 provider 无 SanitizeFunc
	otherTypes := []string{
		config.EndpointTypeOpenAI,
		config.EndpointTypeClaude,
		config.EndpointTypeGemini,
		config.EndpointTypeDualProtocol,
		config.EndpointTypeOpenAIImage,
	}
	for _, epType := range otherTypes {
		p := registry.Lookup(epType)
		if p == nil {
			t.Fatalf("provider %q not registered", epType)
		}
		if p.Body.SanitizeFunc != nil {
			t.Errorf("%s should not have SanitizeFunc", epType)
		}
	}
}

// ---------------------------------------------------------------------------
// TestBodyPolicySanitizeFuncBehavior — Azure SanitizeFunc 实际行为
// ---------------------------------------------------------------------------

func TestBodyPolicySanitizeFuncBehavior(t *testing.T) {
	registry := DefaultProviderRegistry()
	azure := registry.Lookup(config.EndpointTypeAzureOpenAI)
	if azure == nil {
		t.Fatal("azure_openai not registered")
	}
	if azure.Body.SanitizeFunc == nil {
		t.Fatal("azure_openai SanitizeFunc is nil")
	}

	// 构造一个包含 Azure 不兼容字段的请求体
	// "custom_field" 不在 Azure 白名单中，应被剥离
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"custom_field":"should_be_stripped"}`)
	req, _ := http.NewRequest("POST", "http://proxy/v1/chat/completions", bytes.NewReader(body))

	sanitized, stripped := azure.Body.SanitizeFunc(req, body)
	if len(stripped) == 0 {
		t.Error("expected some fields to be stripped for Azure")
	}

	// 验证 stripped 字段不在 sanitized body 中
	var parsed map[string]interface{}
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("sanitized body is not valid JSON: %v", err)
	}
	for _, field := range stripped {
		if _, exists := parsed[field]; exists {
			t.Errorf("stripped field %q still present in sanitized body", field)
		}
	}
}

// ---------------------------------------------------------------------------
// TestBodyPolicyInjectCacheControl — dual_protocol 有 InjectCacheControl，其他无
// ---------------------------------------------------------------------------

func TestBodyPolicyInjectCacheControl(t *testing.T) {
	registry := DefaultProviderRegistry()

	// dual_protocol 有 InjectCacheControl
	dp := registry.Lookup(config.EndpointTypeDualProtocol)
	if dp == nil {
		t.Fatalf("provider %q not registered", config.EndpointTypeDualProtocol)
	}
	if dp.Body.InjectCacheControl == nil {
		t.Errorf("%s should have InjectCacheControl", config.EndpointTypeDualProtocol)
	}

	// 其他 provider 无 InjectCacheControl
	otherTypes := []string{
		config.EndpointTypeOpenAI,
		config.EndpointTypeAzureOpenAI,
		config.EndpointTypeClaude,
		config.EndpointTypeGemini,
		config.EndpointTypeOpenAIImage,
	}
	for _, epType := range otherTypes {
		p := registry.Lookup(epType)
		if p == nil {
			t.Fatalf("provider %q not registered", epType)
		}
		if p.Body.InjectCacheControl != nil {
			t.Errorf("%s should not have InjectCacheControl", epType)
		}
	}
}

// ---------------------------------------------------------------------------
// TestBodyPolicyInjectCacheControlBehavior — Anthropic 路径注入，OpenAI 路径不注入
// ---------------------------------------------------------------------------

func TestBodyPolicyInjectCacheControlBehavior(t *testing.T) {
	registry := DefaultProviderRegistry()
	dp := registry.Lookup(config.EndpointTypeDualProtocol)
	if dp == nil {
		t.Fatal("dual_protocol not registered")
	}
	if dp.Body.InjectCacheControl == nil {
		t.Fatal("dual_protocol InjectCacheControl is nil")
	}

	// 构造 >= 3 轮非 system 消息的请求体（触发注入条件）
	body := []byte(`{
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"hello"},
			{"role":"user","content":"thanks"}
		]
	}`)

	t.Run("Anthropic_path_injects", func(t *testing.T) {
		result := dp.Body.InjectCacheControl(body, "/v1/messages")
		// 验证 cache_control 被注入
		var parsed map[string]interface{}
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("result is not valid JSON: %v", err)
		}
		messages, ok := parsed["messages"].([]interface{})
		if !ok {
			t.Fatal("messages not found in result")
		}
		lastMsg := messages[len(messages)-1].(map[string]interface{})
		content, ok := lastMsg["content"].([]interface{})
		if !ok {
			t.Fatal("last message content should be an array after injection")
		}
		lastBlock := content[len(content)-1].(map[string]interface{})
		if _, has := lastBlock["cache_control"]; !has {
			t.Error("cache_control should be injected for Anthropic path")
		}
	})

	t.Run("OpenAI_path_no_injection", func(t *testing.T) {
		result := dp.Body.InjectCacheControl(body, "/v1/chat/completions")
		// 验证无变化（应返回原始 body）
		if !bytes.Equal(result, body) {
			t.Error("OpenAI path should not inject cache_control")
		}
	})
}
