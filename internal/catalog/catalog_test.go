package catalog

import (
	"encoding/json"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestNew(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	all := c.ListAll()
	if len(all) == 0 {
		t.Fatal("catalog is empty")
	}

	// 验证至少有三种 endpoint_type
	types := map[string]bool{}
	for _, e := range all {
		types[e.EndpointType] = true
	}
	for _, et := range []string{config.EndpointTypeOpenAI, config.EndpointTypeAzureOpenAI, config.EndpointTypeClaude, config.EndpointTypeBailian} {
		if !types[et] {
			t.Errorf("missing endpoint_type %q in catalog", et)
		}
	}
}

func TestLookup(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	tests := []struct {
		name         string
		endpointType string
		model        string
		wantNil      bool
	}{
		{"openai gpt-4o", "openai", "gpt-4o", false},
		{"case insensitive", "OPENAI", "GPT-4O", false},
		{"claude sonnet 4", "claude", "claude-sonnet-4-20250514", false},
		{"azure openai gpt-4o", "azure_openai", "gpt-4o", false},
		{"bailian qwen", "bailian", "qwen3.7-max", false},
		{"bailian qwen case insensitive", "BAILIAN", "QWEN3.7-MAX", false},
		{"nonexistent", "openai", "nonexistent-model", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := c.Lookup(tt.endpointType, tt.model)
			if tt.wantNil && entry != nil {
				t.Error("expected nil, got entry")
			}
			if !tt.wantNil && entry == nil {
				t.Errorf("Lookup(%q, %q) returned nil", tt.endpointType, tt.model)
			}
		})
	}
}

func TestLookupAlias(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// claude-sonnet-4 is an alias for claude-sonnet-4-20250514
	entry := c.Lookup("claude", "claude-sonnet-4")
	if entry == nil {
		t.Fatal("Lookup(claude, claude-sonnet-4) returned nil — alias not working")
	}
	if entry.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected canonical model claude-sonnet-4-20250514, got %s", entry.Model)
	}

	// claude-3.5-sonnet-20241022 -> claude-3-5-sonnet-20241022
	entry = c.Lookup("claude", "claude-3.5-sonnet-20241022")
	if entry == nil {
		t.Fatal("Lookup(claude, claude-3.5-sonnet-20241022) returned nil — dot alias not working")
	}
	if entry.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("expected canonical model claude-3-5-sonnet-20241022, got %s", entry.Model)
	}
}

func TestLookupDefaultCost(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	cost := c.LookupDefaultCost("openai", "gpt-4o")
	if cost == nil {
		t.Fatal("gpt-4o should have default cost")
	}
	if cost.InputPer1MTokens <= 0 {
		t.Error("expected positive input cost")
	}
	if cost.OutputPer1MTokens <= 0 {
		t.Error("expected positive output cost")
	}
}

func TestListByEndpointType(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	openaiModels := c.ListByEndpointType("openai")
	if len(openaiModels) == 0 {
		t.Error("no openai models found")
	}

	claudeModels := c.ListByEndpointType("claude")
	if len(claudeModels) == 0 {
		t.Error("no claude models found")
	}

	azureModels := c.ListByEndpointType("azure_openai")
	if len(azureModels) == 0 {
		t.Error("no azure_openai models found")
	}

	bailianModels := c.ListByEndpointType("bailian")
	if len(bailianModels) == 0 {
		t.Error("no bailian models found")
	}
	foundQwen := false
	for _, m := range bailianModels {
		if m.Model == "qwen3.7-max" {
			foundQwen = true
			break
		}
	}
	if !foundQwen {
		t.Error("bailian catalog should include qwen3.7-max")
	}

	// ListByEndpointType should be case insensitive
	upper := c.ListByEndpointType("OPENAI")
	if len(upper) != len(openaiModels) {
		t.Errorf("case insensitive ListByEndpointType mismatch: %d vs %d", len(upper), len(openaiModels))
	}
}

func TestResolveAlias(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// 非别名应原样返回（小写化）
	resolved := c.ResolveAlias("openai", "gpt-4o")
	if resolved != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %s", resolved)
	}

	// 别名应解析为规范名
	resolved = c.ResolveAlias("claude", "claude-sonnet-4")
	if resolved != "claude-sonnet-4-20250514" {
		t.Errorf("expected claude-sonnet-4-20250514, got %s", resolved)
	}

	// DeepSeek 兼容别名（官方 2026-07-24 弃用）应解析为 v4 规范名
	if got := c.ResolveAlias("deepseek", "deepseek-chat"); got != "deepseek-v4-flash" {
		t.Errorf("expected deepseek-v4-flash, got %s", got)
	}
	if got := c.ResolveAlias("deepseek", "deepseek-reasoner"); got != "deepseek-v4-pro" {
		t.Errorf("expected deepseek-v4-pro, got %s", got)
	}
}

func TestNewFromData(t *testing.T) {
	// 测试从自定义 JSON 创建目录
	entries := []ModelEntry{
		{
			EndpointType: "openai",
			Model:        "test-model",
			DisplayName:  "Test Model",
			Aliases:      []string{"tm-alias"},
			DefaultCost: &Cost{
				InputPer1MTokens:  1.0,
				OutputPer1MTokens: 2.0,
			},
		},
	}

	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	c, err := newFromData(data)
	if err != nil {
		t.Fatalf("newFromData: %v", err)
	}

	// 精确查找
	entry := c.Lookup("openai", "test-model")
	if entry == nil {
		t.Fatal("Lookup(openai, test-model) returned nil")
	}
	if entry.DisplayName != "Test Model" {
		t.Errorf("expected display_name 'Test Model', got %q", entry.DisplayName)
	}

	// 别名查找
	entry = c.Lookup("openai", "tm-alias")
	if entry == nil {
		t.Fatal("alias lookup returned nil")
	}
	if entry.Model != "test-model" {
		t.Errorf("expected model 'test-model' via alias, got %q", entry.Model)
	}

	// 费用
	cost := c.LookupDefaultCost("openai", "test-model")
	if cost == nil {
		t.Fatal("expected non-nil cost")
	}
	if cost.InputPer1MTokens != 1.0 {
		t.Errorf("expected input cost 1.0, got %f", cost.InputPer1MTokens)
	}

	// ListAll
	all := c.ListAll()
	if len(all) != 1 {
		t.Errorf("expected 1 entry, got %d", len(all))
	}
}

func TestNewFromDataInvalidJSON(t *testing.T) {
	_, err := newFromData([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}
