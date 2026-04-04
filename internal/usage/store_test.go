package usage

import (
	"testing"
)

func TestCostTableLookupCost(t *testing.T) {
	costs := CostTable{
		"azure_openai:gpt-4o": {InputPer1MTokens: 0.01, OutputPer1MTokens: 0.02},
		"openai:gpt-4o":       {InputPer1MTokens: 0.05, OutputPer1MTokens: 0.10},
		"gpt-4o":              {InputPer1MTokens: 0.03, OutputPer1MTokens: 0.06},
	}

	// Exact match: azure_openai:gpt-4o
	rate, ok := costs.LookupCost("azure_openai", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.01 {
		t.Fatalf("expected exact match for azure_openai:gpt-4o, got ok=%v rate=%+v", ok, rate)
	}

	// Exact match: openai:gpt-4o
	rate, ok = costs.LookupCost("openai", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.05 {
		t.Fatalf("expected exact match for openai:gpt-4o, got ok=%v rate=%+v", ok, rate)
	}

	// Fallback to model-only when endpoint_type doesn't have exact match
	rate, ok = costs.LookupCost("claude", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.03 {
		t.Fatalf("expected fallback match for claude:gpt-4o to model-only, got ok=%v rate=%+v", ok, rate)
	}

	// Fallback when endpoint_type is empty
	rate, ok = costs.LookupCost("", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 0.03 {
		t.Fatalf("expected fallback match for empty endpoint_type, got ok=%v rate=%+v", ok, rate)
	}

	// Not found
	_, ok = costs.LookupCost("openai", "gpt-unknown")
	if ok {
		t.Fatalf("expected no match for unknown model")
	}
}
