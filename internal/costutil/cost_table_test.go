package costutil

import (
	"testing"

	"github.com/ycgame/llms-proxy/internal/catalog"
	"github.com/ycgame/llms-proxy/internal/nosql"
)

// TestToCostTable_Layer1Only verifies catalog entries are loaded into the table
// when no custom costs are provided.
func TestToCostTable_Layer1Only(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	table := ToCostTable(nil, cat)
	// gpt-4o should be in catalog with default cost
	rate, ok := table.LookupCost("openai", "gpt-4o")
	if !ok {
		t.Fatal("expected catalog default cost for openai:gpt-4o")
	}
	if rate.InputPer1MTokens <= 0 || rate.OutputPer1MTokens <= 0 {
		t.Fatalf("expected positive rates, got %+v", rate)
	}
}

// TestToCostTable_Layer2Override verifies custom costs override catalog defaults.
func TestToCostTable_Layer2Override(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	customCosts := []nosql.ModelCost{
		{EndpointType: "openai", Model: "gpt-4o", InputPer1MTokens: 999, OutputPer1MTokens: 888},
	}
	table := ToCostTable(customCosts, cat)
	rate, ok := table.LookupCost("openai", "gpt-4o")
	if !ok {
		t.Fatal("expected cost for openai:gpt-4o after custom override")
	}
	if rate.InputPer1MTokens != 999 || rate.OutputPer1MTokens != 888 {
		t.Fatalf("expected custom rates (999, 888), got %+v", rate)
	}
}

// TestToCostTable_Layer2NewModel verifies custom costs for models not in catalog.
func TestToCostTable_Layer2NewModel(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	customCosts := []nosql.ModelCost{
		{EndpointType: "openai", Model: "custom-model-xyz", InputPer1MTokens: 42, OutputPer1MTokens: 84},
	}
	table := ToCostTable(customCosts, cat)
	rate, ok := table.LookupCost("openai", "custom-model-xyz")
	if !ok {
		t.Fatal("expected cost for custom-model-xyz")
	}
	if rate.InputPer1MTokens != 42 || rate.OutputPer1MTokens != 84 {
		t.Fatalf("expected (42, 84), got %+v", rate)
	}
}

// TestToCostTable_DualKey verifies both "epType:model" and "model" keys are written.
func TestToCostTable_DualKey(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	table := ToCostTable(nil, cat)
	// "openai:gpt-4o" key
	rate1, ok1 := table.LookupCost("openai", "gpt-4o")
	if !ok1 {
		t.Fatal("expected cost for openai:gpt-4o (composite key)")
	}
	// "gpt-4o" fallback key (should also exist from catalog layer)
	rate2, ok2 := table["gpt-4o"]
	if !ok2 {
		t.Fatal("expected gpt-4o fallback key in table")
	}
	if rate1.InputPer1MTokens != rate2.InputPer1MTokens {
		t.Fatalf("rates mismatch: composite=%+v, fallback=%+v", rate1, rate2)
	}
}

// TestToCostTable_CustomNoEpType verifies custom cost without endpoint_type writes model-only key.
func TestToCostTable_CustomNoEpType(t *testing.T) {
	customCosts := []nosql.ModelCost{
		{Model: "gpt-4o", InputPer1MTokens: 100, OutputPer1MTokens: 200},
	}
	table := ToCostTable(customCosts, nil)
	// Should be accessible by model-only fallback
	rate, ok := table["gpt-4o"]
	if !ok {
		t.Fatal("expected gpt-4o key with no epType")
	}
	if rate.InputPer1MTokens != 100 || rate.OutputPer1MTokens != 200 {
		t.Fatalf("expected (100, 200), got %+v", rate)
	}
	// Should NOT be accessible via composite key lookup with non-empty epType
	_, ok = table["openai:gpt-4o"]
	if ok {
		t.Fatal("should not have openai:gpt-4o key when custom has no epType")
	}
}

// TestToCostTable_NilCatalog verifies nil catalog only uses custom costs.
func TestToCostTable_NilCatalog(t *testing.T) {
	customCosts := []nosql.ModelCost{
		{EndpointType: "openai", Model: "gpt-4o", InputPer1MTokens: 55, OutputPer1MTokens: 66},
	}
	table := ToCostTable(customCosts, nil)
	rate, ok := table.LookupCost("openai", "gpt-4o")
	if !ok || rate.InputPer1MTokens != 55 {
		t.Fatalf("expected custom rate with nil catalog, got ok=%v rate=%+v", ok, rate)
	}
	// gpt-4o-mini not in custom, should not exist
	_, ok = table.LookupCost("openai", "gpt-4o-mini")
	if ok {
		t.Fatal("expected no cost for gpt-4o-mini with nil catalog and no custom entry")
	}
}

// TestToCostTable_EmptyCosts verifies empty custom costs returns only catalog prices.
func TestToCostTable_EmptyCosts(t *testing.T) {
	cat, err := catalog.New()
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	table := ToCostTable([]nosql.ModelCost{}, cat)
	rate, ok := table.LookupCost("openai", "gpt-4o")
	if !ok {
		t.Fatal("expected catalog default for gpt-4o with empty costs")
	}
	if rate.InputPer1MTokens <= 0 {
		t.Fatalf("expected positive rate, got %+v", rate)
	}
}
