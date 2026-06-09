package nosql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/ycgame/llms-proxy/internal/config"
)

// legacyTargetJSON is the on-disk bbolt format used to detect and migrate
// old targets that stored allowed_models (now replaced by model_mappings).
// It embeds the current config.Target plus the legacy AllowedModels slice.
// Because Go's standard json.Unmarshal allows the same JSON key to populate
// both an embedded field and a shadowed sibling, AllowedModels is populated
// from the pre-migration "allowed_models" JSON array while ModelMappings is
// populated from the new "model_mappings" array. When AllowedModels is non-empty
// and ModelMappings is empty, the List() loop migrates the record.
type legacyTargetJSON struct {
	config.Target
	AllowedModels []string `json:"allowed_models"`
}

var targetOrderKey = []byte("target_order")

// TargetStore manages upstream targets backed by bbolt.
type TargetStore struct {
	db *bolt.DB
}

// NewTargetStore creates a new bbolt-backed target store.
func NewTargetStore(db *bolt.DB) *TargetStore {
	return &TargetStore{db: db}
}

// MigrateFromConfig copies legacy config targets into this store when empty.
func (s *TargetStore) MigrateFromConfig(targets []config.Target) (bool, error) {
	return MigrateTargetsFromConfig(s.db, targets)
}

// List returns all targets in configured order.
func (s *TargetStore) List() ([]config.Target, error) {
	targetsByKey := make(map[string]config.Target)
	var keys []string
	var order []string
	var changed []targetWriteback

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b == nil {
			return nil
		}
		if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
			_ = json.Unmarshal(meta.Get(targetOrderKey), &order)
		}
		return b.ForEach(func(k, v []byte) error {
			key := string(k)
			var target config.Target
			if err := json.Unmarshal(v, &target); err != nil {
				return fmt.Errorf("decode target %q: %w", key, err)
			}
			// Migrate legacy allowed_models → model_mappings when the old field
			// is observed in raw bytes but ModelMappings is empty. The legacy
			// struct is embedded so json.Unmarshal reads AllowedModels alongside
			// the new ModelMappings field; we only act when ModelMappings is empty.
			var legacy legacyTargetJSON
			migrated := false
			if err := json.Unmarshal(v, &legacy); err == nil && len(legacy.AllowedModels) > 0 && len(legacy.Target.ModelMappings) == 0 {
				target = legacy.Target
				target.ModelMappings = make([]config.ModelMapping, 0, len(legacy.AllowedModels))
				for _, m := range legacy.AllowedModels {
					m = strings.TrimSpace(m)
					if m == "" {
						continue
					}
					target.ModelMappings = append(target.ModelMappings, config.ModelMapping{Upstream: m})
				}
				migrated = true
			}
			normalized := normalizeTarget(target)
			if migrated || targetJSONChanged(target, normalized) {
				changed = append(changed, targetWriteback{
					key:        key,
					observed:   append([]byte(nil), v...),
					normalized: normalized,
				})
			}
			targetsByKey[key] = normalized
			keys = append(keys, key)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		if err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(BucketTargets))
			for _, write := range changed {
				if err := putTargetIfUnchanged(b, write); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	seen := make(map[string]struct{}, len(targetsByKey))
	targets := make([]config.Target, 0, len(targetsByKey))
	for _, key := range order {
		key = strings.ToLower(strings.TrimSpace(key))
		target, ok := targetsByKey[key]
		if !ok {
			continue
		}
		targets = append(targets, target)
		seen[key] = struct{}{}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		targets = append(targets, targetsByKey[key])
	}
	return cloneTargets(targets), nil
}

// Create appends a new target.
func (s *TargetStore) Create(target config.Target) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	target = normalizeTarget(target)
	key := strings.ToLower(target.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b.Get([]byte(key)) != nil {
			return fmt.Errorf("target %q already exists", target.Name)
		}
		if err := putTarget(b, key, target); err != nil {
			return err
		}
		return updateTargetOrder(tx, func(order []string) []string {
			return appendMissingTargetOrder(order, key)
		})
	})
}

// Update updates one target by name.
func (s *TargetStore) Update(name string, target config.Target) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	oldKey := strings.ToLower(name)
	next := normalizeTarget(target)
	newKey := strings.ToLower(next.Name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b.Get([]byte(oldKey)) == nil {
			return fmt.Errorf("target %q not found", name)
		}
		if newKey != oldKey && b.Get([]byte(newKey)) != nil {
			return fmt.Errorf("target %q already exists", next.Name)
		}
		if newKey != oldKey {
			if err := b.Delete([]byte(oldKey)); err != nil {
				return err
			}
		}
		if err := putTarget(b, newKey, next); err != nil {
			return err
		}
		return updateTargetOrder(tx, func(order []string) []string {
			replaced := false
			for i, key := range order {
				if key == oldKey {
					order[i] = newKey
					replaced = true
				}
			}
			if !replaced {
				order = append(order, newKey)
			}
			return dedupeTargetOrder(order)
		})
	})
}

// Delete removes one target by name.
func (s *TargetStore) Delete(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	key := strings.ToLower(name)

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("target %q not found", name)
		}
		if err := b.Delete([]byte(key)); err != nil {
			return err
		}
		return updateTargetOrder(tx, func(order []string) []string {
			next := order[:0]
			for _, existing := range order {
				if existing != key {
					next = append(next, existing)
				}
			}
			return next
		})
	})
}

// MigrateTargetsFromConfig copies legacy config.json targets into bbolt when
// the target bucket is empty. It returns true when targets were copied.
func MigrateTargetsFromConfig(db *bolt.DB, targets []config.Target) (bool, error) {
	if len(targets) == 0 {
		return false, nil
	}

	normalized := make([]config.Target, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if err := validateTarget(target); err != nil {
			return false, err
		}
		target = normalizeTarget(target)
		key := strings.ToLower(target.Name)
		if _, ok := seen[key]; ok {
			return false, fmt.Errorf("target %q already exists", target.Name)
		}
		seen[key] = struct{}{}
		normalized = append(normalized, target)
	}

	migrated := false
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTargets))
		k, _ := b.Cursor().First()
		if k != nil {
			return nil
		}
		order := make([]string, 0, len(normalized))
		for _, target := range normalized {
			key := strings.ToLower(target.Name)
			if err := putTarget(b, key, target); err != nil {
				return err
			}
			order = append(order, key)
		}
		if err := writeTargetOrder(tx, order); err != nil {
			return err
		}
		migrated = true
		return nil
	})
	return migrated, err
}

func putTarget(b *bolt.Bucket, key string, target config.Target) error {
	data, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("encode target: %w", err)
	}
	return b.Put([]byte(key), data)
}

func validateTarget(target config.Target) error {
	name := strings.TrimSpace(target.Name)
	if name == "" {
		return errors.New("name must not be empty")
	}
	epType := config.NormalizeEndpointType(target.EndpointType)
	if !config.IsValidEndpointType(epType) {
		return errors.New("invalid endpoint_type")
	}
	if strings.TrimSpace(target.Endpoint) == "" {
		return errors.New("endpoint must not be empty")
	}
	hasAnyKey := strings.TrimSpace(target.APIKey) != ""
	for _, key := range target.APIKeys {
		if strings.TrimSpace(key) != "" {
			hasAnyKey = true
			break
		}
	}
	if !hasAnyKey && !target.AllowBearer {
		return errors.New("api_key must not be empty when allow_bearer_passthrough is false")
	}
	return nil
}

func normalizeTarget(target config.Target) config.Target {
	target = config.NormalizeTargetForCompatibility(target)
	target.Name = strings.TrimSpace(target.Name)
	target.EndpointType = config.NormalizeEndpointType(target.EndpointType)
	target.Endpoint = strings.TrimSpace(target.Endpoint)
	target.APIKey = strings.TrimSpace(target.APIKey)
	target.KeyResetTime = strings.TrimSpace(target.KeyResetTime)
	target.ProviderClass = strings.TrimSpace(target.ProviderClass)
	target.AuthMode = strings.TrimSpace(target.AuthMode)
	target.ImageOperation = config.NormalizeImageOperation(target.ImageOperation)
	for i := range target.APIKeys {
		target.APIKeys[i] = strings.TrimSpace(target.APIKeys[i])
	}
	for i := range target.ModelMappings {
		target.ModelMappings[i].Upstream = strings.TrimSpace(target.ModelMappings[i].Upstream)
		target.ModelMappings[i].Fallback = strings.TrimSpace(target.ModelMappings[i].Fallback)
	}
	return target
}

func targetJSONChanged(a, b config.Target) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return true
	}
	return string(ab) != string(bb)
}

type targetWriteback struct {
	key        string
	observed   []byte
	normalized config.Target
}

func putTargetIfUnchanged(b *bolt.Bucket, write targetWriteback) error {
	if b == nil {
		return nil
	}
	if !bytes.Equal(b.Get([]byte(write.key)), write.observed) {
		return nil
	}
	return putTarget(b, write.key, write.normalized)
}

func updateTargetOrder(tx *bolt.Tx, update func([]string) []string) error {
	var order []string
	if meta := tx.Bucket([]byte(BucketMeta)); meta != nil {
		_ = json.Unmarshal(meta.Get(targetOrderKey), &order)
	}
	return writeTargetOrder(tx, update(order))
}

func writeTargetOrder(tx *bolt.Tx, order []string) error {
	meta := tx.Bucket([]byte(BucketMeta))
	if meta == nil {
		return nil
	}
	data, err := json.Marshal(dedupeTargetOrder(order))
	if err != nil {
		return err
	}
	return meta.Put(targetOrderKey, data)
}

func appendMissingTargetOrder(order []string, key string) []string {
	for _, existing := range order {
		if existing == key {
			return order
		}
	}
	return append(order, key)
}

func dedupeTargetOrder(order []string) []string {
	seen := make(map[string]struct{}, len(order))
	next := make([]string, 0, len(order))
	for _, key := range order {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		next = append(next, key)
	}
	return next
}

func cloneTargets(targets []config.Target) []config.Target {
	if targets == nil {
		return nil
	}
	cloned := make([]config.Target, len(targets))
	for i := range targets {
		cloned[i] = targets[i]
		if len(targets[i].APIKeys) > 0 {
			cloned[i].APIKeys = append([]string(nil), targets[i].APIKeys...)
		}
		if len(targets[i].ModelMappings) > 0 {
			cloned[i].ModelMappings = append([]config.ModelMapping(nil), targets[i].ModelMappings...)
		}
	}
	return cloned
}
