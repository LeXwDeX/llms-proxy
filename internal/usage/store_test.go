package usage

import (
	"testing"
	"time"
)

func TestFilterEvents(t *testing.T) {
	now := time.Now().UTC()
	events := []Event{
		{Timestamp: now.Add(-2 * time.Hour), ClientName: "alice", Model: "gpt-4"},
		{Timestamp: now.Add(-1 * time.Hour), ClientName: "bob", Model: "gpt-4"},
		{Timestamp: now, ClientName: "alice", Model: "gpt-3.5"},
		{Timestamp: now.Add(1 * time.Hour), ClientName: "charlie", Model: "gpt-4"},
	}

	t.Run("no filter", func(t *testing.T) {
		result := FilterEvents(events, Filter{})
		if len(result) != 4 {
			t.Errorf("expected 4 events, got %d", len(result))
		}
	})

	t.Run("filter by client", func(t *testing.T) {
		result := FilterEvents(events, Filter{ClientName: "alice"})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("filter by model case insensitive", func(t *testing.T) {
		result := FilterEvents(events, Filter{Model: "GPT-4"})
		if len(result) != 3 {
			t.Errorf("expected 3 events, got %d", len(result))
		}
	})

	t.Run("filter by time range", func(t *testing.T) {
		from := now.Add(-90 * time.Minute)
		to := now.Add(30 * time.Minute)
		result := FilterEvents(events, Filter{From: &from, To: &to})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("filter by from only", func(t *testing.T) {
		from := now.Add(-30 * time.Minute)
		result := FilterEvents(events, Filter{From: &from})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("filter by to only", func(t *testing.T) {
		to := now.Add(-30 * time.Minute)
		result := FilterEvents(events, Filter{To: &to})
		if len(result) != 2 {
			t.Errorf("expected 2 events, got %d", len(result))
		}
	})

	t.Run("combined filter", func(t *testing.T) {
		result := FilterEvents(events, Filter{ClientName: "alice", Model: "gpt-4"})
		if len(result) != 1 {
			t.Errorf("expected 1 event, got %d", len(result))
		}
	})

	t.Run("empty events", func(t *testing.T) {
		result := FilterEvents(nil, Filter{ClientName: "alice"})
		if len(result) != 0 {
			t.Errorf("expected 0 events, got %d", len(result))
		}
	})
}

func TestNormalizeGroupBy(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hour", "hour"},
		{"HOUR", "hour"},
		{" Hour ", "hour"},
		{"day", "day"},
		{"DAY", "day"},
		{"", "day"},
		{"week", "day"},
		{"month", "day"},
	}

	for _, tt := range tests {
		result := NormalizeGroupBy(tt.input)
		if result != tt.expected {
			t.Errorf("NormalizeGroupBy(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestBucketStartFor(t *testing.T) {
	t.Run("hour grouping", func(t *testing.T) {
		ts := time.Date(2024, 3, 15, 14, 35, 42, 123456789, time.UTC)
		result := BucketStartFor(ts, "hour")
		expected := time.Date(2024, 3, 15, 14, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("BucketStartFor hour = %v, want %v", result, expected)
		}
	})

	t.Run("day grouping", func(t *testing.T) {
		ts := time.Date(2024, 3, 15, 14, 35, 42, 123456789, time.UTC)
		result := BucketStartFor(ts, "day")
		expected := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("BucketStartFor day = %v, want %v", result, expected)
		}
	})

	t.Run("converts to UTC", func(t *testing.T) {
		loc := time.FixedZone("CST", 8*3600)
		ts := time.Date(2024, 3, 15, 22, 30, 0, 0, loc) // 2024-03-15 14:30 UTC
		result := BucketStartFor(ts, "hour")
		expected := time.Date(2024, 3, 15, 14, 0, 0, 0, time.UTC)
		if !result.Equal(expected) {
			t.Errorf("BucketStartFor with timezone = %v, want %v", result, expected)
		}
	})
}

func TestStepDuration(t *testing.T) {
	if StepDuration("hour") != time.Hour {
		t.Errorf("StepDuration(hour) = %v, want %v", StepDuration("hour"), time.Hour)
	}
	if StepDuration("day") != 24*time.Hour {
		t.Errorf("StepDuration(day) = %v, want %v", StepDuration("day"), 24*time.Hour)
	}
	if StepDuration("") != 24*time.Hour {
		t.Errorf("StepDuration('') = %v, want %v", StepDuration(""), 24*time.Hour)
	}
}

func TestSortedDimensions(t *testing.T) {
	input := map[string]*DimensionTotals{
		"charlie": {Key: "charlie", Totals: Totals{Requests: 3}},
		"alice":   {Key: "alice", Totals: Totals{Requests: 1}},
		"bob":     {Key: "bob", Totals: Totals{Requests: 2}},
	}

	result := SortedDimensions(input)
	if len(result) != 3 {
		t.Fatalf("expected 3 dimensions, got %d", len(result))
	}

	// Should be sorted alphabetically
	if result[0].Key != "alice" {
		t.Errorf("expected first key to be 'alice', got %q", result[0].Key)
	}
	if result[1].Key != "bob" {
		t.Errorf("expected second key to be 'bob', got %q", result[1].Key)
	}
	if result[2].Key != "charlie" {
		t.Errorf("expected third key to be 'charlie', got %q", result[2].Key)
	}

	// Verify totals are preserved
	if result[0].Requests != 1 {
		t.Errorf("expected alice requests=1, got %d", result[0].Requests)
	}
}

func TestSortedDimensionsEmpty(t *testing.T) {
	result := SortedDimensions(map[string]*DimensionTotals{})
	if len(result) != 0 {
		t.Errorf("expected 0 dimensions, got %d", len(result))
	}
}

func TestAddEventTotals(t *testing.T) {
	costs := CostTable{
		"openai:gpt-4": {
			InputPer1MTokens:      30.0,
			OutputPer1MTokens:     60.0,
			CachedInputPer1MToken: 15.0,
		},
	}

	target := &Totals{}
	evt := Event{
		EndpointType: "openai",
		Model:        "gpt-4",
		InputTokens:  1000,
		OutputTokens: 500,
		CachedTokens: 200,
	}

	AddEventTotals(target, evt, costs)

	if target.Requests != 1 {
		t.Errorf("expected requests=1, got %d", target.Requests)
	}
	if target.InputTokens != 1000 {
		t.Errorf("expected input_tokens=1000, got %d", target.InputTokens)
	}
	if target.OutputTokens != 500 {
		t.Errorf("expected output_tokens=500, got %d", target.OutputTokens)
	}
	if target.CachedTokens != 200 {
		t.Errorf("expected cached_tokens=200, got %d", target.CachedTokens)
	}

	// Expected cost: (1000/1M * 30) + (500/1M * 60) + (200/1M * 15) = 0.03 + 0.03 + 0.003 = 0.063
	expectedCost := 0.063
	if target.EstimatedCost < expectedCost-0.001 || target.EstimatedCost > expectedCost+0.001 {
		t.Errorf("expected estimated_cost≈%.3f, got %.6f", expectedCost, target.EstimatedCost)
	}

	// Add another event
	evt2 := Event{
		EndpointType: "openai",
		Model:        "gpt-4",
		InputTokens:  2000,
		OutputTokens: 1000,
		CachedTokens: 0,
	}
	AddEventTotals(target, evt2, costs)

	if target.Requests != 2 {
		t.Errorf("expected requests=2, got %d", target.Requests)
	}
	if target.InputTokens != 3000 {
		t.Errorf("expected input_tokens=3000, got %d", target.InputTokens)
	}
}

func TestAddEventTotalsNoCost(t *testing.T) {
	costs := CostTable{} // No rates defined

	target := &Totals{}
	evt := Event{
		EndpointType: "unknown",
		Model:        "unknown-model",
		InputTokens:  1000,
		OutputTokens: 500,
	}

	AddEventTotals(target, evt, costs)

	if target.Requests != 1 {
		t.Errorf("expected requests=1, got %d", target.Requests)
	}
	if target.EstimatedCost != 0 {
		t.Errorf("expected estimated_cost=0, got %.6f", target.EstimatedCost)
	}
}

func TestEstimateEventCost(t *testing.T) {
	costs := CostTable{
		"gpt-4": {
			InputPer1MTokens:      30.0,
			OutputPer1MTokens:     60.0,
			CachedInputPer1MToken: 15.0,
		},
	}

	t.Run("with cost rates", func(t *testing.T) {
		evt := Event{
			Model:        "gpt-4",
			InputTokens:  1_000_000,
			OutputTokens: 1_000_000,
			CachedTokens: 1_000_000,
		}
		cost := EstimateEventCost(evt, costs)
		// Expected: 30 + 60 + 15 = 105
		if cost < 104.9 || cost > 105.1 {
			t.Errorf("expected cost≈105, got %.2f", cost)
		}
	})

	t.Run("no cost rates", func(t *testing.T) {
		evt := Event{Model: "unknown"}
		cost := EstimateEventCost(evt, costs)
		if cost != 0 {
			t.Errorf("expected cost=0, got %.2f", cost)
		}
	})

	t.Run("zero tokens", func(t *testing.T) {
		evt := Event{Model: "gpt-4"}
		cost := EstimateEventCost(evt, costs)
		if cost != 0 {
			t.Errorf("expected cost=0, got %.2f", cost)
		}
	})
}

func TestLookupCost(t *testing.T) {
	costs := CostTable{
		"openai:gpt-4": {InputPer1MTokens: 30.0},
		"gpt-3.5":      {InputPer1MTokens: 1.0},
	}

	t.Run("exact match with endpoint type", func(t *testing.T) {
		rates, ok := costs.LookupCost("openai", "gpt-4")
		if !ok {
			t.Fatal("expected to find rates")
		}
		if rates.InputPer1MTokens != 30.0 {
			t.Errorf("expected 30.0, got %.1f", rates.InputPer1MTokens)
		}
	})

	t.Run("fallback to model only", func(t *testing.T) {
		rates, ok := costs.LookupCost("azure", "gpt-3.5")
		if !ok {
			t.Fatal("expected to find rates via fallback")
		}
		if rates.InputPer1MTokens != 1.0 {
			t.Errorf("expected 1.0, got %.1f", rates.InputPer1MTokens)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := costs.LookupCost("openai", "gpt-5")
		if ok {
			t.Error("expected not to find rates")
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		rates, ok := costs.LookupCost("OPENAI", "GPT-4")
		if !ok {
			t.Fatal("expected to find rates (case insensitive)")
		}
		if rates.InputPer1MTokens != 30.0 {
			t.Errorf("expected 30.0, got %.1f", rates.InputPer1MTokens)
		}
	})

	t.Run("empty endpoint type", func(t *testing.T) {
		rates, ok := costs.LookupCost("", "gpt-3.5")
		if !ok {
			t.Fatal("expected to find rates with empty endpoint type")
		}
		if rates.InputPer1MTokens != 1.0 {
			t.Errorf("expected 1.0, got %.1f", rates.InputPer1MTokens)
		}
	})
}

func TestInferOriginalEndpointType(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		// ── Global providers ──

		// Anthropic Claude
		{"claude-sonnet-4-20250514", "claude"},
		{"claude-opus-4-1", "claude"},
		{"claude-3-5-haiku-20241022", "claude"},
		{"Claude_Sonnet_4", "claude"},

		// Google Gemini + Gemma
		{"gemini-2.0-flash", "gemini"},
		{"gemini-1.5-pro", "gemini"},
		{"Gemini_Pro", "gemini"},
		{"gemma-3-27b-it", "gemini"},
		{"gemma-4-31b-it", "gemini"},

		// OpenAI (gpt, o-series, dall-e, embedding, tts, whisper, chatgpt, codex)
		{"gpt-4o", "openai"},
		{"gpt-5.5", "openai"},
		{"gpt-4.1-mini", "openai"},
		{"o1-mini", "openai"},
		{"o3-mini", "openai"},
		{"o4-mini", "openai"},
		{"dall-e-3", "openai"},
		{"text-embedding-3-large", "openai"},
		{"tts-1", "openai"},
		{"tts-1-hd", "openai"},
		{"whisper-1", "openai"},
		{"chatgpt-image-latest", "openai"},
		{"codex-mini", "openai"},

		// Mistral AI (mistral, ministral, codestral)
		{"mistral-large-2411", "mistral"},
		{"ministral-3b", "mistral"},
		{"codestral-2501", "mistral"},
		{"mistral-nemo", "mistral"},

		// xAI Grok
		{"grok-3", "grok"},
		{"grok-4", "grok"},
		{"grok-code-fast-1", "grok"},

		// Cohere
		{"cohere-command-r-08-2024", "cohere"},
		{"cohere-embed-v3-english", "cohere"},

		// Meta Llama
		{"llama-3.3-70b-instruct", "meta"},
		{"llama-4-scout-17b-16e-instruct", "meta"},
		{"meta-llama-3-70b-instruct", "meta"},

		// Microsoft Phi
		{"phi-3-mini-128k-instruct", "phi"},
		{"phi-4", "phi"},
		{"phi-4-reasoning", "phi"},

		// ── Chinese providers ──

		// DeepSeek
		{"deepseek-v4-pro", "deepseek"},
		{"deepseek-v4-flash", "deepseek"},
		{"DeepSeek_Chat", "deepseek"},

		// 智谱AI GLM
		{"glm-4", "glm"},
		{"glm-4-plus", "glm"},
		{"glm-4v", "glm"},
		{"GLM_4_Flash", "glm"},

		// MiniMax (minimax-*, abab*)
		{"minimax-text-01", "minimax"},
		{"abab6.5s-chat", "minimax"},
		{"abab5.5-chat", "minimax"},

		// 阿里通义千问 Qwen
		{"qwen-turbo", "qwen"},
		{"qwen-plus", "qwen"},
		{"qwen-max", "qwen"},
		{"qwen3-235b-a22b", "qwen"},
		{"qwen3.7-max", "qwen"},
		{"qwen2.5-72b-instruct", "qwen"},

		// 月之暗面 Kimi / Moonshot
		{"kimi-k2-thinking", "kimi"},
		{"kimi-k2.5", "kimi"},
		{"moonshot-v1-8k", "kimi"},

		// 零一万物 Yi
		{"yi-large", "yi"},
		{"yi-lightning", "yi"},
		{"yi-vision", "yi"},

		// 百川智能 Baichuan
		{"baichuan2-turbo", "baichuan"},
		{"baichuan4", "baichuan"},

		// 阶跃星辰 Step
		{"step-1-8k", "step"},
		{"step-2-16k", "step"},

		// 上海AI Lab 书生 InternLM
		{"internlm2-chat-7b", "internlm"},
		{"internlm3-8b-instruct", "internlm"},

		// 字节跳动豆包 Doubao
		{"doubao-pro-32k", "doubao"},
		{"doubao-lite-4k", "doubao"},

		// 百度文心 Ernie
		{"ernie-4.0-8k", "ernie"},
		{"ernie-speed-128k", "ernie"},

		// 腾讯混元 Hunyuan
		{"hunyuan-pro", "hunyuan"},
		{"hunyuan-standard", "hunyuan"},

		// 科大讯飞星火 Spark
		{"spark-pro", "spark"},
		{"spark-max", "spark"},

		// Unknown / unmapped
		{"model-router", ""},
		{"mai-ds-r1", ""},
		{"", ""},
		{"  ", ""},
	}
	for _, tt := range tests {
		got := InferOriginalEndpointType(tt.model)
		if got != tt.want {
			t.Errorf("InferOriginalEndpointType(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

func TestLookupCostDualProtocol(t *testing.T) {
	// Simulate a cost table built by toUsageCostTable with catalog entries
	// under original endpoint_types (no dual_protocol entries).
	costs := CostTable{
		// Claude model under claude endpoint_type
		"claude:claude-sonnet-4-20250514": {InputPer1MTokens: 3.0, OutputPer1MTokens: 15.0},
		"claude-sonnet-4-20250514":        {InputPer1MTokens: 5.0, OutputPer1MTokens: 25.0}, // model-only (overwritten by azure_openai)
		// Same model also under azure_openai (simulating catalog duplicate)
		"azure_openai:claude-sonnet-4-20250514": {InputPer1MTokens: 5.0, OutputPer1MTokens: 25.0},
		// GPT model under openai
		"openai:gpt-4o": {InputPer1MTokens: 2.5, OutputPer1MTokens: 10.0},
		"gpt-4o":        {InputPer1MTokens: 5.0, OutputPer1MTokens: 20.0}, // overwritten by azure_openai
		"azure_openai:gpt-4o": {InputPer1MTokens: 5.0, OutputPer1MTokens: 20.0},
		// DeepSeek model
		"deepseek:deepseek-v4-pro": {InputPer1MTokens: 0.5, OutputPer1MTokens: 2.0},
		"deepseek-v4-pro":          {InputPer1MTokens: 0.5, OutputPer1MTokens: 2.0},
		// Custom dual_protocol override (should take priority)
		"dual_protocol:claude-opus-4-1": {InputPer1MTokens: 99.0, OutputPer1MTokens: 99.0},
		"claude:claude-opus-4-1":        {InputPer1MTokens: 15.0, OutputPer1MTokens: 75.0},
	}

	t.Run("dual_protocol uses original manufacturer pricing over model-only fallback", func(t *testing.T) {
		// claude-sonnet-4-20250514 via dual_protocol should use claude:* pricing (3.0/15.0)
		// NOT the model-only fallback (5.0/25.0 which was overwritten by azure_openai)
		rates, ok := costs.LookupCost("dual_protocol", "claude-sonnet-4-20250514")
		if !ok {
			t.Fatal("expected to find rates via dual_protocol smart fallback")
		}
		if rates.InputPer1MTokens != 3.0 || rates.OutputPer1MTokens != 15.0 {
			t.Errorf("expected claude pricing (3.0/15.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("dual_protocol gpt model uses openai pricing", func(t *testing.T) {
		rates, ok := costs.LookupCost("dual_protocol", "gpt-4o")
		if !ok {
			t.Fatal("expected to find rates via dual_protocol smart fallback")
		}
		if rates.InputPer1MTokens != 2.5 || rates.OutputPer1MTokens != 10.0 {
			t.Errorf("expected openai pricing (2.5/10.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("dual_protocol deepseek model uses deepseek pricing", func(t *testing.T) {
		rates, ok := costs.LookupCost("dual_protocol", "deepseek-v4-pro")
		if !ok {
			t.Fatal("expected to find rates via dual_protocol smart fallback")
		}
		if rates.InputPer1MTokens != 0.5 || rates.OutputPer1MTokens != 2.0 {
			t.Errorf("expected deepseek pricing (0.5/2.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("dual_protocol custom override takes priority", func(t *testing.T) {
		// Custom dual_protocol:claude-opus-4-1 should beat claude:claude-opus-4-1
		rates, ok := costs.LookupCost("dual_protocol", "claude-opus-4-1")
		if !ok {
			t.Fatal("expected to find rates")
		}
		if rates.InputPer1MTokens != 99.0 || rates.OutputPer1MTokens != 99.0 {
			t.Errorf("expected custom dual_protocol pricing (99.0/99.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("dual_protocol qwen model uses qwen pricing", func(t *testing.T) {
		costsWithQwen := CostTable{
			"qwen:qwen3.7-max": {InputPer1MTokens: 0.8, OutputPer1MTokens: 2.0},
			"qwen3.7-max":      {InputPer1MTokens: 1.5, OutputPer1MTokens: 5.0}, // model-only (different)
		}
		rates, ok := costsWithQwen.LookupCost("dual_protocol", "qwen3.7-max")
		if !ok {
			t.Fatal("expected to find rates via qwen inference")
		}
		if rates.InputPer1MTokens != 0.8 || rates.OutputPer1MTokens != 2.0 {
			t.Errorf("expected qwen pricing (0.8/2.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("dual_protocol glm model uses glm pricing", func(t *testing.T) {
		costsWithGLM := CostTable{
			"glm:glm-4-plus": {InputPer1MTokens: 4.0, OutputPer1MTokens: 12.0},
			"glm-4-plus":     {InputPer1MTokens: 6.0, OutputPer1MTokens: 18.0},
		}
		rates, ok := costsWithGLM.LookupCost("dual_protocol", "glm-4-plus")
		if !ok {
			t.Fatal("expected to find rates via glm inference")
		}
		if rates.InputPer1MTokens != 4.0 || rates.OutputPer1MTokens != 12.0 {
			t.Errorf("expected glm pricing (4.0/12.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("dual_protocol minimax model uses minimax pricing", func(t *testing.T) {
		costsWithMM := CostTable{
			"minimax:abab6.5s-chat": {InputPer1MTokens: 1.0, OutputPer1MTokens: 3.0},
			"abab6.5s-chat":         {InputPer1MTokens: 2.0, OutputPer1MTokens: 6.0},
		}
		rates, ok := costsWithMM.LookupCost("dual_protocol", "abab6.5s-chat")
		if !ok {
			t.Fatal("expected to find rates via minimax inference")
		}
		if rates.InputPer1MTokens != 1.0 || rates.OutputPer1MTokens != 3.0 {
			t.Errorf("expected minimax pricing (1.0/3.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("dual_protocol truly unknown model falls back to model-only", func(t *testing.T) {
		// model-router has no inferred original type, should fall back to model-only
		costsWithUnknown := CostTable{
			"model-router": {InputPer1MTokens: 0.5, OutputPer1MTokens: 1.5},
		}
		rates, ok := costsWithUnknown.LookupCost("dual_protocol", "model-router")
		if !ok {
			t.Fatal("expected to find rates via model-only fallback")
		}
		if rates.InputPer1MTokens != 0.5 || rates.OutputPer1MTokens != 1.5 {
			t.Errorf("expected model-only pricing (0.5/1.5), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})

	t.Run("non-dual_protocol unchanged", func(t *testing.T) {
		// Verify that non-dual_protocol lookups are not affected
		rates, ok := costs.LookupCost("openai", "gpt-4o")
		if !ok {
			t.Fatal("expected to find rates for openai:gpt-4o")
		}
		if rates.InputPer1MTokens != 2.5 || rates.OutputPer1MTokens != 10.0 {
			t.Errorf("expected openai pricing (2.5/10.0), got (%.1f/%.1f)",
				rates.InputPer1MTokens, rates.OutputPer1MTokens)
		}
	})
}
