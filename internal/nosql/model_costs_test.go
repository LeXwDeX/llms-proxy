package nosql

import (
	"path/filepath"
	"testing"
)

func TestModelCostStoreUpsertAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_costs.json")
	store := NewModelCostStore(path)

	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1KTokens: 0.1, OutputPer1KTokens: 0.2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1KTokens: 0.3, OutputPer1KTokens: 0.4}); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].InputPer1KTokens != 0.3 {
		t.Fatalf("unexpected costs: %+v", items)
	}

	if err := store.Delete("gpt-4o"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	items, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty costs, got %+v", items)
	}
}
