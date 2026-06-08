package nosql

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ycgame/llms-proxy/internal/config"
	bolt "go.etcd.io/bbolt"
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

func TestTargetStoreListNormalizesAndPersistsLegacyEndpointTypes(t *testing.T) {
	db := testDB(t)
	legacy := config.Target{
		Name:         "legacy-edit",
		EndpointType: "wangsu_openai_image_edit",
		Endpoint:     "https://image.example.com/v1/images/edits",
		APIKey:       "key",
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		data, err := json.Marshal(legacy)
		if err != nil {
			return err
		}
		return b.Put([]byte("legacy-edit"), data)
	}); err != nil {
		t.Fatalf("seed legacy target: %v", err)
	}

	store := NewTargetStore(db)
	targets, err := store.List()
	if err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if len(targets) != 1 || targets[0].EndpointType != config.EndpointTypeOpenAIImage || targets[0].ImageOperation != config.ImageOperationEdits {
		t.Fatalf("legacy target not normalized: %+v", targets)
	}

	targets, err = store.List()
	if err != nil {
		t.Fatalf("list targets again: %v", err)
	}
	if len(targets) != 1 || targets[0].EndpointType != config.EndpointTypeOpenAIImage || targets[0].ImageOperation != config.ImageOperationEdits {
		t.Fatalf("persisted target not normalized: %+v", targets)
	}
}

func TestTargetWritebackSkipsWhenObservedRawChanged(t *testing.T) {
	db := testDB(t)
	legacy := config.Target{
		Name:         "legacy-edit",
		EndpointType: "wangsu_openai_image_edit",
		Endpoint:     "https://image.example.com/v1/images/edits",
		APIKey:       "key",
	}
	updated := config.Target{
		Name:              "legacy-edit",
		EndpointType:      config.EndpointTypeOpenAI,
		Endpoint:          "https://updated.example.com/v1",
		APIKey:            "updated-key",
		ModelMappings: []config.ModelMapping{{Upstream: "gpt-4o"}},
		ProviderClass:     "updated-provider",
		KeyResetTime:      "00:00",
		SupportsResponses: true,
	}

	var observed []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		data, err := json.Marshal(legacy)
		if err != nil {
			return err
		}
		observed = append([]byte(nil), data...)
		return b.Put([]byte("legacy-edit"), data)
	}); err != nil {
		t.Fatalf("seed legacy target: %v", err)
	}
	if err := NewTargetStore(db).Update("legacy-edit", updated); err != nil {
		t.Fatalf("update target before writeback: %v", err)
	}

	normalized := normalizeTarget(legacy)
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		return putTargetIfUnchanged(b, targetWriteback{key: "legacy-edit", observed: observed, normalized: normalized})
	}); err != nil {
		t.Fatalf("conditional writeback: %v", err)
	}

	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		current := b.Get([]byte("legacy-edit"))
		updatedRaw, err := json.Marshal(normalizeTarget(updated))
		if err != nil {
			return err
		}
		if !bytes.Equal(current, updatedRaw) {
			t.Fatalf("writeback overwrote updated target: %s", current)
		}
		return nil
	}); err != nil {
		t.Fatalf("read target after writeback: %v", err)
	}
}
