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

func TestModelCostStoreEndpointTypeDimension(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// Upsert same model with different endpoint types.
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
	if len(items) != 2 {
		t.Fatalf("expected 2 costs (different endpoint_types), got %d: %+v", len(items), items)
	}

	// Upsert should replace only the matching endpoint_type + model.
	if err := store.Upsert(ModelCost{EndpointType: "openai", Model: "gpt-4o", InputPer1MTokens: 0.9}); err != nil {
		t.Fatalf("upsert replace openai: %v", err)
	}
	items, err = store.List()
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 costs after replace, got %d", len(items))
	}
	for _, item := range items {
		if item.EndpointType == "openai" && item.InputPer1MTokens != 0.9 {
			t.Fatalf("expected openai cost to be updated to 0.9, got %f", item.InputPer1MTokens)
		}
	}

	// DeleteByKey should remove only the specific endpoint_type + model.
	if err := store.DeleteByKey("openai", "gpt-4o"); err != nil {
		t.Fatalf("delete by key: %v", err)
	}
	items, err = store.List()
	if err != nil {
		t.Fatalf("list after delete by key: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 cost after delete by key, got %d", len(items))
	}
	if items[0].EndpointType != "azure_openai" {
		t.Fatalf("expected remaining cost to be azure_openai, got %q", items[0].EndpointType)
	}
}

func TestModelCostStoreEmptyEndpointTypeDefaultsToAzureOpenAI(t *testing.T) {
	db := testDB(t)
	store := NewModelCostStore(db)

	// Empty endpoint_type should default to azure_openai.
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
	if items[0].EndpointType != "azure_openai" {
		t.Fatalf("expected default endpoint_type azure_openai, got %q", items[0].EndpointType)
	}

	// DeleteByKey with empty endpoint_type should default to azure_openai and match.
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
