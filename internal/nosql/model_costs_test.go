package nosql

import (
	"testing"
)

func TestModelCostStoreUpsertAndDelete(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.1, OutputPer1MTokens: 0.2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.3, OutputPer1MTokens: 0.4}); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].InputPer1MTokens != 0.3 {
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

func TestModelCostStoreEndpointTypeOverwrites(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// The key is model-only; upserting the same model with different
	// endpoint_types overwrites (last-writer-wins).
	if err := store.Upsert(ModelCost{EndpointType: "azure_openai", Model: "gpt-4o", InputPer1MTokens: 0.1}); err != nil {
		t.Fatalf("upsert azure: %v", err)
	}
	if err := store.Upsert(ModelCost{EndpointType: "openai", Model: "gpt-4o", InputPer1MTokens: 0.5}); err != nil {
		t.Fatalf("upsert openai: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 cost (model-only key, last writer wins), got %d: %+v", len(items), items)
	}
	if items[0].InputPer1MTokens != 0.5 {
		t.Fatalf("expected overwritten rate 0.5, got %f", items[0].InputPer1MTokens)
	}

	// DeleteByKey delegates to Delete.
	if err := store.DeleteByKey("openai", "gpt-4o"); err != nil {
		t.Fatalf("delete by key: %v", err)
	}
	items, err = store.List()
	if err != nil {
		t.Fatalf("list after delete by key: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 costs after delete by key, got %d", len(items))
	}
}

func TestModelCostStoreEmptyEndpointTypeNotDefaulted(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// Empty endpoint_type is kept as-is (no longer defaulted to azure_openai).
	if err := store.Upsert(ModelCost{Model: "gpt-4o", InputPer1MTokens: 0.1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 cost, got %d", len(items))
	}
	if items[0].EndpointType != "" {
		t.Fatalf("expected empty endpoint_type, got %q", items[0].EndpointType)
	}

	// DeleteByKey with empty endpoint_type delegates to Delete by model.
	if err := store.DeleteByKey("", "gpt-4o"); err != nil {
		t.Fatalf("delete by key with empty endpoint_type: %v", err)
	}
	items, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty costs after delete, got %+v", items)
	}
}
