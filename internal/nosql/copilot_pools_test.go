package nosql

import (
	"testing"
)

func TestCopilotPoolStore_CRUD(t *testing.T) {
	db := testDB(t)
	store := NewCopilotPoolStore(db)

	// Create
	err := store.Create(CopilotPool{
		Name:       "pool-alpha",
		ClientName: "client-a",
		Targets:    []string{"Target1", " target2 ", "TARGET1"}, // duplicates & whitespace
		Notes:      "test pool",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// List
	pools, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(pools))
	}
	p := pools[0]
	if p.Name != "pool-alpha" {
		t.Errorf("name = %q, want %q", p.Name, "pool-alpha")
	}
	if p.ClientName != "client-a" {
		t.Errorf("client_name = %q, want %q", p.ClientName, "client-a")
	}
	// Targets should be normalized and deduplicated
	if len(p.Targets) != 2 {
		t.Errorf("targets = %v, want 2 unique targets", p.Targets)
	}
	if p.UpdatedAt == "" {
		t.Error("expected UpdatedAt to be set")
	}

	// Get
	got, err := store.Get("pool-alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "pool-alpha" || got.ClientName != "client-a" {
		t.Fatalf("get returned unexpected pool: %+v", got)
	}

	// Get case-insensitive
	got2, err := store.Get("POOL-ALPHA")
	if err != nil {
		t.Fatalf("get case-insensitive: %v", err)
	}
	if got2.Name != "pool-alpha" {
		t.Fatalf("get case-insensitive returned %q", got2.Name)
	}

	// Update (rename + change client_name)
	err = store.Update("pool-alpha", CopilotPool{
		Name:       "pool-beta",
		ClientName: "client-b",
		Targets:    []string{"target3"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	pools, err = store.List()
	if err != nil {
		t.Fatalf("list after update: %v", err)
	}
	if len(pools) != 1 || pools[0].Name != "pool-beta" || pools[0].ClientName != "client-b" {
		t.Fatalf("unexpected pools after update: %+v", pools)
	}

	// Old name should be gone
	_, err = store.Get("pool-alpha")
	if err == nil {
		t.Fatal("expected error getting old name after rename")
	}

	// Delete
	err = store.Delete("pool-beta")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	pools, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("expected 0 pools, got %d", len(pools))
	}
}

func TestCopilotPoolStore_NameUnique(t *testing.T) {
	db := testDB(t)
	store := NewCopilotPoolStore(db)

	if err := store.Create(CopilotPool{Name: "pool-x", ClientName: "client-1"}); err != nil {
		t.Fatalf("create first: %v", err)
	}
	// Duplicate name (same case)
	if err := store.Create(CopilotPool{Name: "pool-x", ClientName: "client-2"}); err == nil {
		t.Fatal("expected duplicate name error")
	}
	// Duplicate name (different case)
	if err := store.Create(CopilotPool{Name: "POOL-X", ClientName: "client-3"}); err == nil {
		t.Fatal("expected case-insensitive duplicate name error")
	}
}

func TestCopilotPoolStore_ClientNameUnique(t *testing.T) {
	db := testDB(t)
	store := NewCopilotPoolStore(db)

	if err := store.Create(CopilotPool{Name: "pool-a", ClientName: "shared-client"}); err != nil {
		t.Fatalf("create pool-a: %v", err)
	}
	// Different pool, same client_name
	err := store.Create(CopilotPool{Name: "pool-b", ClientName: "shared-client"})
	if err == nil {
		t.Fatal("expected client_name uniqueness error on create")
	}
	// Different pool, same client_name different case
	err = store.Create(CopilotPool{Name: "pool-c", ClientName: "SHARED-CLIENT"})
	if err == nil {
		t.Fatal("expected case-insensitive client_name uniqueness error")
	}
}

func TestCopilotPoolStore_ClientNameUniqueOnUpdate(t *testing.T) {
	db := testDB(t)
	store := NewCopilotPoolStore(db)

	if err := store.Create(CopilotPool{Name: "pool-1", ClientName: "client-a"}); err != nil {
		t.Fatalf("create pool-1: %v", err)
	}
	if err := store.Create(CopilotPool{Name: "pool-2", ClientName: "client-b"}); err != nil {
		t.Fatalf("create pool-2: %v", err)
	}

	// Update pool-2 to use client-a (already used by pool-1)
	err := store.Update("pool-2", CopilotPool{Name: "pool-2", ClientName: "client-a"})
	if err == nil {
		t.Fatal("expected client_name uniqueness error on update")
	}

	// Updating pool-1 keeping its own client_name should succeed
	err = store.Update("pool-1", CopilotPool{Name: "pool-1", ClientName: "client-a", Notes: "updated"})
	if err != nil {
		t.Fatalf("update own client_name should succeed: %v", err)
	}
}

func TestCopilotPoolStore_DeleteNotFound(t *testing.T) {
	db := testDB(t)
	store := NewCopilotPoolStore(db)

	err := store.Delete("nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent pool")
	}
}

func TestCopilotPoolStore_UpdateNotFound(t *testing.T) {
	db := testDB(t)
	store := NewCopilotPoolStore(db)

	err := store.Update("nonexistent", CopilotPool{Name: "nonexistent", ClientName: "c"})
	if err == nil {
		t.Fatal("expected error updating nonexistent pool")
	}
}

func TestCopilotPoolStore_ValidationErrors(t *testing.T) {
	db := testDB(t)
	store := NewCopilotPoolStore(db)

	// Empty name
	if err := store.Create(CopilotPool{Name: "", ClientName: "c"}); err == nil {
		t.Fatal("expected error for empty name")
	}
	// Empty client_name
	if err := store.Create(CopilotPool{Name: "p", ClientName: ""}); err == nil {
		t.Fatal("expected error for empty client_name")
	}
	// Whitespace-only name
	if err := store.Create(CopilotPool{Name: "  ", ClientName: "c"}); err == nil {
		t.Fatal("expected error for whitespace-only name")
	}
}
