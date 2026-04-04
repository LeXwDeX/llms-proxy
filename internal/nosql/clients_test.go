package nosql

import (
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestClientStoreCRUD(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	if err := store.Create(config.Client{Name: "team-a", AccessKey: "k1"}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := store.Create(config.Client{Name: "team-a", AccessKey: "k2"}); err == nil {
		t.Fatalf("expected duplicate name error")
	}

	clients, err := store.List()
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "team-a" {
		t.Fatalf("unexpected clients: %+v", clients)
	}

	if err := store.Update("team-a", config.Client{Name: "team-b", AccessKey: "k2", AllowedTargets: []string{"Primary"}}); err != nil {
		t.Fatalf("update client: %v", err)
	}

	clients, err = store.List()
	if err != nil {
		t.Fatalf("list after update: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "team-b" || clients[0].AllowedTargets[0] != "primary" {
		t.Fatalf("unexpected updated clients: %+v", clients)
	}

	if err := store.Delete("team-b"); err != nil {
		t.Fatalf("delete client: %v", err)
	}

	clients, err = store.List()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("expected no clients, got %+v", clients)
	}
}

func TestClientStoreAccessKeyUniqueness(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	if err := store.Create(config.Client{Name: "a", AccessKey: "k1"}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := store.Create(config.Client{Name: "b", AccessKey: "k1"}); err == nil {
		t.Fatalf("expected duplicate access_key error")
	}
}

func TestClientStoreCaseInsensitive(t *testing.T) {
	db := testDB(t)
	store := NewClientStore(db)

	if err := store.Create(config.Client{Name: "Team-A", AccessKey: "k1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Creating with different case should fail.
	if err := store.Create(config.Client{Name: "team-a", AccessKey: "k2"}); err == nil {
		t.Fatalf("expected case-insensitive duplicate name error")
	}

	// Update with different case should work.
	if err := store.Update("TEAM-A", config.Client{Name: "Team-B", AccessKey: "k2"}); err != nil {
		t.Fatalf("update with different case: %v", err)
	}

	// Delete with different case should work.
	if err := store.Delete("TEAM-B"); err != nil {
		t.Fatalf("delete with different case: %v", err)
	}
}
