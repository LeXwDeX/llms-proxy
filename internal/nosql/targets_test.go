package nosql

import (
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestTargetStoreCRUDPreservesOrder(t *testing.T) {
	db := testDB(t)
	store := NewTargetStore(db)

	first := config.Target{Name: "primary", Endpoint: "https://one.example.com", ResourcePathPrefix: "/", APIKey: "k1"}
	second := config.Target{Name: "secondary", Endpoint: "https://two.example.com", ResourcePathPrefix: "/", APIKey: "k2"}

	if err := store.Create(first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := store.Create(second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	if err := store.Create(second); err == nil {
		t.Fatalf("expected duplicate target error")
	}

	targets, err := store.List()
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 2 || targets[0].Name != "primary" || targets[1].Name != "secondary" {
		t.Fatalf("unexpected target order: %+v", targets)
	}

	second.Endpoint = "https://updated.example.com"
	if err := store.Update("secondary", second); err != nil {
		t.Fatalf("update target: %v", err)
	}
	if err := store.Delete("primary"); err != nil {
		t.Fatalf("delete target: %v", err)
	}

	targets, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(targets) != 1 || targets[0].Name != "secondary" || targets[0].Endpoint != second.Endpoint {
		t.Fatalf("unexpected targets after update/delete: %+v", targets)
	}
}

func TestMigrateTargetsFromConfig(t *testing.T) {
	db := testDB(t)
	legacy := []config.Target{
		{Name: "first", Endpoint: "https://one.example.com", ResourcePathPrefix: "/", APIKey: "k1"},
		{Name: "second", Endpoint: "https://two.example.com", ResourcePathPrefix: "/", APIKey: "k2"},
	}

	migrated, err := MigrateTargetsFromConfig(db, legacy)
	if err != nil {
		t.Fatalf("migrate targets: %v", err)
	}
	if !migrated {
		t.Fatalf("expected migration to copy targets")
	}

	targets, err := NewTargetStore(db).List()
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 2 || targets[0].Name != "first" || targets[1].Name != "second" {
		t.Fatalf("unexpected migrated targets: %+v", targets)
	}

	migrated, err = MigrateTargetsFromConfig(db, []config.Target{{Name: "third", Endpoint: "https://three.example.com", ResourcePathPrefix: "/", APIKey: "k3"}})
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if migrated {
		t.Fatalf("expected second migration to skip non-empty target store")
	}
}
