package nosql

import (
	"path/filepath"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestClientStoreCRUD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.json")
	store := NewClientStore(path)

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
