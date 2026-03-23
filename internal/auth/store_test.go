package auth

import (
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
)

func TestStoreLoadFromConfigAndAuthenticate(t *testing.T) {
	store := NewStore()
	clients := []config.Client{{
		Name:           "team-alpha",
		AccessKey:      "token123",
		AllowedTargets: []string{"primary"},
	}}

	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}

	principal, ok := store.Authenticate("token123")
	if !ok {
		t.Fatal("expected token to authenticate")
	}
	if principal.Name != "team-alpha" {
		t.Fatalf("unexpected principal name: %s", principal.Name)
	}
	if principal.AllowAll() {
		t.Fatal("expected principal to have restricted targets")
	}
	if !principal.CanAccess("primary") {
		t.Fatal("principal should access primary target")
	}
	if principal.CanAccess("secondary") {
		t.Fatal("principal should not access secondary target")
	}
}

func TestStoreLoadAllowAllClients(t *testing.T) {
	store := NewStore()
	clients := []config.Client{{
		Name:      "ops",
		AccessKey: "ops-token",
		// empty allowed_targets => allow all
	}}

	if err := store.LoadFromConfig(clients); err != nil {
		t.Fatalf("expected load to succeed: %v", err)
	}

	principal, ok := store.Authenticate("ops-token")
	if !ok {
		t.Fatal("expected ops token to authenticate")
	}
	if !principal.AllowAll() {
		t.Fatal("expected empty allowed targets to grant full access")
	}
}

func TestAuthenticateReturnsClone(t *testing.T) {
	store := NewStore()
	if err := store.LoadFromConfig([]config.Client{{
		Name:           "team",
		AccessKey:      "abc",
		AllowedTargets: []string{"primary"},
	}}); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	principal, ok := store.Authenticate("abc")
	if !ok {
		t.Fatal("expected principal")
	}

	clone, ok := store.Authenticate("abc")
	if !ok {
		t.Fatal("expected clone principal")
	}

	if &principal.allowedTargets == &clone.allowedTargets {
		t.Fatal("expected authenticate to return deep copy")
	}
}

func TestStoreLoadFromConfigRejectsDuplicateAccessKey(t *testing.T) {
	store := NewStore()
	err := store.LoadFromConfig([]config.Client{
		{Name: "team-a", AccessKey: "same-key"},
		{Name: "team-b", AccessKey: "same-key"},
	})
	if err == nil {
		t.Fatal("expected duplicate access_key validation error")
	}
}
